package tachyon

import (
	"sync"
	"time"
)

type RunResult struct {
	Task    *Task
	Result  *Result
	Runtime time.Duration
}

type Runner struct {
	env       *Environment
	plays     []*Play
	wait      sync.WaitGroup
	to_notify map[string]struct{}
	async     chan *AsyncAction
	report    Reporter

	Results []RunResult
	Start   time.Time
	Runtime time.Duration
}

func NewRunner(env *Environment, plays []*Play) *Runner {
	r := &Runner{
		env:       env,
		plays:     plays,
		to_notify: make(map[string]struct{}),
		async:     make(chan *AsyncAction),
		report:    env.report,
	}

	go r.handleAsync()

	return r
}

func (r *Runner) SetReport(rep Reporter) {
	r.report = rep
}

func (r *Runner) AddNotify(n string) {
	r.to_notify[n] = struct{}{}
}

func (r *Runner) ShouldRunHandler(name string) bool {
	_, ok := r.to_notify[name]

	return ok
}

func (r *Runner) AsyncChannel() chan *AsyncAction {
	return r.async
}

func (r *Runner) Run(env *Environment) error {
	start := time.Now()
	r.Start = start

	defer func() {
		r.Runtime = time.Since(start)
	}()

	r.report.StartTasks(r)

	for _, play := range r.plays {
		fs := NewFutureScope(play.Vars)

		for _, task := range play.Tasks {
			err := r.runTask(env, play, task, fs, fs)
			if err != nil {
				return err
			}
		}

		r.Results = append(r.Results, fs.Results()...)
	}

	r.report.FinishTasks(r)

	r.wait.Wait()

	r.report.StartHandlers(r)

	for _, play := range r.plays {
		fs := NewFutureScope(play.Vars)

		for _, task := range play.Handlers {
			if r.ShouldRunHandler(task.Name()) {
				err := r.runTask(env, play, task, fs, fs)

				if err != nil {
					return err
				}
			}
		}

		fs.Wait()
	}

	r.report.FinishHandlers(r)

	return nil
}

func RunAdhocTask(cmd, args string) (*Result, error) {
	env := NewEnv(NewNestedScope(nil), &Config{})
	defer env.Cleanup()

	task := AdhocTask(cmd, args)

	str, err := ExpandVars(env.Vars, task.Args())
	if err != nil {
		return nil, err
	}

	obj, err := MakeCommand(env.Vars, task, str)
	if err != nil {
		return nil, err
	}

	ce := &CommandEnv{Env: env, Paths: env.Paths}

	return obj.Run(ce, args)
}

func RunAdhocCommand(cmd Command, args string) (*Result, error) {
	env := NewEnv(NewNestedScope(nil), &Config{})
	defer env.Cleanup()

	ce := &CommandEnv{Env: env, Paths: env.Paths}

	return cmd.Run(ce, args)
}

type PriorityScope struct {
	task Vars
	rest Scope
}

func (p *PriorityScope) Get(key string) (Value, bool) {
	if p.task != nil {
		if v, ok := p.task[key]; ok {
			return Any(v), true
		}
	}

	return p.rest.Get(key)
}

func (p *PriorityScope) Set(key string, val interface{}) {
	p.rest.Set(key, val)
}

func boolify(str string) bool {
	switch str {
	case "", "false", "no":
		return false
	default:
		return true
	}
}

type ModuleRun struct {
	Play        *Play
	Task        *Task
	Module      *Module
	Runner      *Runner
	Scope       Scope
	FutureScope *FutureScope
}

func (m *ModuleRun) Run(env *CommandEnv, args string) (*Result, error) {
	for _, task := range m.Module.ModTasks {
		ns := NewNestedScope(m.Scope)
		sm, err := ParseSimpleMap(ns, args)
		if err != nil {
			return nil, err
		}

		for k, v := range sm {
			ns.Set(k, v)
		}

		m.Runner.runTask(env.Env, m.Play, task, ns, m.FutureScope)
	}

	return NewResult(true), nil
}

func (r *Runner) runTaskItems(env *Environment, play *Play, task *Task, s Scope, fs *FutureScope, start time.Time) error {
	for _, item := range task.Items() {
		ns := NewNestedScope(s)
		ns.Set("item", item)

		name, err := ExpandVars(ns, task.Name())
		if err != nil {
			return err
		}

		str, err := ExpandVars(ns, task.Args())
		if err != nil {
			return err
		}

		cmd, err := MakeCommand(ns, task, str)

		if err != nil {
			return err
		}

		r.report.StartTask(task, cmd, name, str)

		ce := NewCommandEnv(env, task)

		res, err := cmd.Run(ce, str)

		if name := task.Register(); name != "" {
			fs.Set(name, res)
		}

		runtime := time.Since(start)

		if err != nil {
			res = NewResult(false)
			res.Data.Set("failed", true)
			res.Data.Set("error", err.Error())
		}

		r.Results = append(r.Results, RunResult{task, res, runtime})

		r.report.FinishTask(task, cmd, res)

		if err == nil {
			for _, x := range task.Notify() {
				r.AddNotify(x)
			}
		}
	}

	return nil
}

func (r *Runner) runTask(env *Environment, play *Play, task *Task, s Scope, fs *FutureScope) error {
	ps := &PriorityScope{task.IncludeVars, s}

	start := time.Now()

	if when := task.When(); when != "" {
		when, err := ExpandVars(ps, when)

		if err != nil {
			return err
		}

		if !boolify(when) {
			return nil
		}
	}

	if items := task.Items(); items != nil {
		return r.runTaskItems(env, play, task, s, fs, start)
	}

	name, err := ExpandVars(ps, task.Name())
	if err != nil {
		return err
	}

	str, err := ExpandVars(ps, task.Args())
	if err != nil {
		return err
	}

	var cmd Command

	if mod, ok := play.Modules[task.Command()]; ok {
		cmd = &ModuleRun{
			Play:   play,
			Task:   task,
			Module: mod,
			Runner: r,
			Scope:  s,
		}
	} else {
		cmd, err = MakeCommand(ps, task, str)

		if err != nil {
			return err
		}
	}

	r.report.StartTask(task, cmd, name, str)

	ce := NewCommandEnv(env, task)

	if name := task.Future(); name != "" {
		future := NewFuture(start, task, func() (*Result, error) {
			return cmd.Run(ce, str)
		})

		fs.AddFuture(name, future)

		return nil
	}

	if task.Async() {
		asyncAction := &AsyncAction{Task: task}
		asyncAction.Init(r)

		go func() {
			asyncAction.Finish(cmd.Run(ce, str))
		}()
	} else {
		res, err := cmd.Run(ce, str)

		if name := task.Register(); name != "" {
			fs.Set(name, res)
		}

		runtime := time.Since(start)

		if err != nil {
			res = NewResult(false)
			res.Data.Set("failed", true)
			res.Data.Set("error", err.Error())
		}

		r.Results = append(r.Results, RunResult{task, res, runtime})

		r.report.FinishTask(task, cmd, res)

		if err == nil {
			for _, x := range task.Notify() {
				r.AddNotify(x)
			}
		}
	}

	return err
}
