package app

import (
	"errors"
	"fmt"
	"github.com/timeredbull/tsuru/api/auth"
	"github.com/timeredbull/tsuru/api/bind"
	"github.com/timeredbull/tsuru/api/service"
	"github.com/timeredbull/tsuru/config"
	"github.com/timeredbull/tsuru/db"
	"github.com/timeredbull/tsuru/log"
	"github.com/timeredbull/tsuru/repository"
	"labix.org/v2/mgo/bson"
	"launchpad.net/goyaml"
	"os/exec"
	"path"
	"strings"
	"time"
)

const confSep = "========"

type EnvVar bind.EnvVar

func (e *EnvVar) String() string {
	var value, suffix string
	if e.Public {
		value = e.Value
	} else {
		value = "***"
		suffix = " (private variable)"
	}
	return fmt.Sprintf("%s=%s%s", e.Name, value, suffix)
}

type App struct {
	Env         map[string]EnvVar
	Framework   string
	JujuEnv     string
	KeystoneEnv KeystoneEnv
	Logs        []Log
	Name        string
	State       string
	Units       []Unit
	Teams       []string
}

type Log struct {
	Date    time.Time
	Message string
}

type conf struct {
	PreRestart string `yaml:"pre-restart"`
	PosRestart string `yaml:"pos-restart"`
}

func (a *App) Get() error {
	return db.Session.Apps().Find(bson.M{"name": a.Name}).One(&a)
}

// Creates a new app and save it in database
func NewApp(name string, framework string, teams []string) (App, error) {
	a := App{
		Name:      name,
		Framework: framework,
		Teams:     teams,
	}
	// TODO (flaviamissi): check if tsuru is in multi tenant mode before
	// creating a new tenant for an app
	var err error
	a.KeystoneEnv.TenantId, err = NewTenant(&a)
	if err != nil {
		return a, err
	}
	a.KeystoneEnv.UserId, err = NewUser(&a)
	if err != nil {
		return a, err
	}
	var secret string
	a.KeystoneEnv.AccessKey, secret, err = NewEC2Creds(&a)
	_ = secret
	if err != nil {
		return a, err
	}
	a.State = "pending"
	// TODO (#110): make JujuEnv match the app name, and bootstrap it before
	// deploy the app.
	a.JujuEnv = "delta"
	err = db.Session.Apps().Insert(a)
	if err != nil {
		return a, err
	}
	a.Log(fmt.Sprintf("creating app %s", a.Name))
	cmd := exec.Command("juju", "deploy", "-e", a.JujuEnv, "--repository=/home/charms", "local:"+a.Framework, a.Name)
	log.Printf("deploying %s with name %s on environment %s", a.Framework, a.Name, a.JujuEnv)
	out, err := cmd.CombinedOutput()
	a.Log(string(out))
	if err != nil {
		a.Log(fmt.Sprintf("juju finished with exit status: %s", err.Error()))
		db.Session.Apps().Remove(bson.M{"name": a.Name})
		return a, errors.New(string(out))
	}
	a.Log(fmt.Sprintf("app %s successfully created", a.Name))
	return a, nil
}

func (a *App) unbind() error {
	var instances []service.ServiceInstance
	err := db.Session.ServiceInstances().Find(bson.M{"apps": bson.M{"$in": []string{a.Name}}}).All(&instances)
	if err != nil {
		return err
	}
	var msg string
	var addMsg = func(instanceName string, reason error) {
		if msg == "" {
			msg = "Failed to unbind the following instances:\n"
		}
		msg += fmt.Sprintf("- %s (%s)", instanceName, reason.Error())
	}
	for _, instance := range instances {
		err = instance.Unbind(a)
		if err != nil {
			addMsg(instance.Name, err)
		}
	}
	if msg != "" {
		return errors.New(msg)
	}
	return nil
}

func (a *App) Destroy() error {
	unbindingApp := App{Name: a.Name}
	err := unbindingApp.Get()
	if err != nil {
		return err
	}
	if err = destroyKeystoneEnv(&unbindingApp); err != nil {
		return err
	}
	unbindCh := make(chan error)
	go func() {
		unbindCh <- unbindingApp.unbind()
	}()
	err = db.Session.Apps().Remove(bson.M{"name": a.Name})
	if err != nil {
		return err
	}
	out, err := a.unit().Destroy()
	log.Printf(string(out))
	if err != nil {
		return err
	}
	return <-unbindCh
}

func (a *App) AddOrUpdateUnit(u *Unit) {
	for i, unt := range a.Units {
		if unt.Machine == u.Machine {
			a.Units[i] = *u
			return
		}
	}
	a.Units = append(a.Units, *u)
}

func (a *App) findTeam(team *auth.Team) int {
	for i, t := range a.Teams {
		if t == team.Name {
			return i
		}
	}
	return -1
}

func (a *App) hasTeam(team *auth.Team) bool {
	return a.findTeam(team) > -1
}

func (a *App) GrantAccess(team *auth.Team) error {
	if a.hasTeam(team) {
		return errors.New("This team has already access to this app")
	}
	a.Teams = append(a.Teams, team.Name)
	return nil
}

func (a *App) RevokeAccess(team *auth.Team) error {
	index := a.findTeam(team)
	if index < 0 {
		return errors.New("This team does not have access to this app")
	}
	last := len(a.Teams) - 1
	a.Teams[index] = a.Teams[last]
	a.Teams = a.Teams[:last]
	return nil
}

func (a *App) GetTeams() (teams []auth.Team) {
	db.Session.Teams().Find(bson.M{"_id": bson.M{"$in": a.Teams}}).All(&teams)
	return
}

func (a *App) setTeams(teams []auth.Team) {
	a.Teams = make([]string, len(teams))
	for i, team := range teams {
		a.Teams[i] = team.Name
	}
}

func (a *App) SetEnv(env EnvVar) {
	if a.Env == nil {
		a.Env = make(map[string]EnvVar)
	}
	a.Env[env.Name] = env
	a.Log(fmt.Sprintf("setting env %s with value %s", env.Name, env.Value))
}

func (a *App) GetEnv(name string) (env EnvVar, err error) {
	var ok bool
	if env, ok = a.Env[name]; !ok {
		err = errors.New("Environment variable not declared for this app.")
	}
	return
}

func (a *App) InstanceEnv(name string) map[string]bind.EnvVar {
	envs := make(map[string]bind.EnvVar)
	for k, env := range a.Env {
		if env.InstanceName == name {
			envs[k] = bind.EnvVar(env)
		}
	}
	return envs
}

func deployHookAbsPath(p string) (string, error) {
	repoPath, err := config.GetString("git:unit-repo")
	if err != nil {
		return "", nil
	}
	return path.Join(repoPath, p), nil
}

/*
* Returns app.conf located at app's git repository
 */
func (a *App) conf() (conf, error) {
	var c conf
	uRepo, err := repository.GetPath()
	if err != nil {
		a.Log(fmt.Sprintf("Got error while getting repository path: %s", err.Error()))
		return c, err
	}
	cPath := path.Join(uRepo, "app.conf")
	cmd := fmt.Sprintf(`echo "%s";cat %s`, confSep, cPath)
	o, err := a.unit().Command(cmd)
	if err != nil {
		a.Log(fmt.Sprintf("Got error while executing command: %s... Skipping hooks execution", err.Error()))
		return c, nil
	}
	data := strings.Split(string(o), confSep)[1]
	err = goyaml.Unmarshal([]byte(data), &c)
	if err != nil {
		a.Log(fmt.Sprintf("Got error while parsing yaml: %s", err.Error()))
		return c, err
	}
	return c, nil
}

/*
* preRestart is responsible for running user's pre-restart script.
* The path to this script can be found at the app.conf file, at the root of user's app repository.
 */
func (a *App) preRestart(c conf) ([]byte, error) {
	if !a.hasRestartHooks(c) {
		msg := "app.conf file does not exists or is in the right place. Skipping pre-restart hook..."
		a.Log(msg)
		return []byte(msg + "\n"), nil
	}
	if c.PreRestart == "" {
		msg := "pre-restart hook section in app conf does not exists... Skipping pre-restart hook..."
		a.Log(msg)
		return []byte(msg + "\n"), nil
	}
	p, err := deployHookAbsPath(c.PreRestart)
	if err != nil {
		msg := fmt.Sprintf("Error obtaining absolute path to hook: %s. Skipping pre-restart hook...", err)
		a.Log(msg)
		return []byte(msg + "\n"), nil
	}
	a.Log("Executing pre-restart hook...")
	out, err := a.unit().Command("/bin/bash", p)
	a.Log(fmt.Sprintf("Output of pre-restart hook: %s", string(out)))
	return out, err
}

/*
* posRestart is responsible for running user's pos-restart script.
* The path to this script can be found at the app.conf file, at the root of user's app repository.
 */
func (a *App) posRestart(c conf) ([]byte, error) {
	if !a.hasRestartHooks(c) {
		msg := "app.conf file does not exists or is in the right place. Skipping pos-restart hook..."
		a.Log(msg)
		return []byte(msg + "\n"), nil
	}
	if c.PosRestart == "" {
		msg := "pos-restart hook section in app conf does not exists... Skipping pos-restart hook..."
		a.Log(msg)
		return []byte(msg + "\n"), nil
	}
	p, err := deployHookAbsPath(c.PosRestart)
	if err != nil {
		msg := fmt.Sprintf("Error obtaining absolute path to hook: %s. Skipping pos-restart-hook...", err)
		a.Log(msg)
		return []byte(msg + "\n"), nil
	}
	out, err := a.unit().Command("/bin/bash", p)
	a.Log("Executing pos-restart hook...")
	a.Log(fmt.Sprintf("Output of pos-restart hook: %s", string(out)))
	return out, err
}

func (a *App) hasRestartHooks(c conf) bool {
	return !(c.PreRestart == "" && c.PosRestart == "")
}

func (a *App) updateHooks() ([]byte, error) {
	u := a.unit()
	a.Log("executting hook dependencies")
	out, err := u.ExecuteHook("dependencies")
	a.Log(string(out))
	if err != nil {
		return out, err
	}
	a.Log("executting hook to restarting")
	out, err = u.ExecuteHook("restart")
	if err != nil {
		return out, err
	}
	a.Log(string(out))
	return out, nil
}

func (a *App) unit() *Unit {
	if len(a.Units) > 0 {
		unit := a.Units[0]
		unit.app = a
		return &unit
	}
	return &Unit{app: a}
}

func (a *App) GetUnits() []bind.Unit {
	var units []bind.Unit
	for _, u := range a.Units {
		u.app = a
		units = append(units, bind.Unit(&u))
	}
	return units
}

func (a *App) GetName() string {
	return a.Name
}

func (a *App) SetEnvs(envs []bind.EnvVar, publicOnly bool) error {
	e := make([]EnvVar, len(envs))
	for i, env := range envs {
		e[i] = EnvVar(env)
	}
	return SetEnvsToApp(a, e, publicOnly)
}

func (a *App) UnsetEnvs(envs []string, publicOnly bool) error {
	return UnsetEnvFromApp(a, envs, publicOnly)
}

func (a *App) Log(message string) error {
	log.Printf(message)
	l := Log{Date: time.Now(), Message: message}
	a.Logs = append(a.Logs, l)
	return db.Session.Apps().Update(bson.M{"name": a.Name}, a)
}
