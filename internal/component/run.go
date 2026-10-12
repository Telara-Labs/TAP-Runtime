package component

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/sys"
)

// Config is what a component may see of the host. It mirrors what a WASI
// Preview 1 guest is given: arguments, the three standard streams, clocks and
// random bytes. There is deliberately no field for directories, environment
// variables or network access.
type Config struct {
	Args     []string
	Stdin    io.Reader
	Stdout   io.Writer
	Stderr   io.Writer
	Walltime func() (sec int64, nsec int32)
	Nanotime func() int64
	Rand     io.Reader
}

// Program is a loaded component: its core modules compiled, its wiring
// checked, ready to run once in the runtime it was loaded into.
type Program struct {
	rt       wazero.Runtime
	space    *space
	compiled map[int]wazero.CompiledModule
	run      coreItem
	post     *coreItem
}

// Load parses and checks a component and compiles its core modules in rt.
// Every import is resolved here, so a component that imports something TAP
// does not provide is refused before any of it runs.
func Load(ctx context.Context, rt wazero.Runtime, b []byte) (p *Program, err error) {
	defer catch(&err)
	s := parseComponent(b)
	p = &Program{rt: rt, space: s, compiled: map[int]wazero.CompiledModule{}}
	run := findRun(s)
	p.run, p.post = *run.lifted, run.postReturn
	checkMemories(s)
	for k, ci := range s.coreInstances {
		if !ci.real {
			continue
		}
		mod := s.coreModules[ci.module]
		ci.imports = make([][2]string, len(mod.imports))
		ci.hosts, ci.sigs = nil, nil
		for i, imp := range mod.imports {
			src, ok := ci.args[imp.module]
			if !ok {
				fail("core module import %q is not supplied by the component", imp.module)
			}
			var it coreItem
			if from := s.coreInstances[src]; from.real {
				it = coreItem{inst: src, name: imp.field}
			} else if it, ok = from.exports[imp.field]; !ok {
				fail("core module import %s.%s is not supplied by the component", imp.module, imp.field)
			}
			if it.host == nil {
				ci.imports[i] = [2]string{instName(it.inst), it.name}
				continue
			}
			if imp.kind != 0x00 {
				fail("core module import %s.%s must be a function", imp.module, imp.field)
			}
			checkSignature(it.host, *imp.sig)
			ci.imports[i] = [2]string{hostName(k), "f" + strconv.Itoa(len(ci.hosts))}
			ci.hosts = append(ci.hosts, it.host)
			ci.sigs = append(ci.sigs, *imp.sig)
		}
		cm, cerr := rt.CompileModule(ctx, mod.rewrite(ci.imports))
		if cerr != nil {
			return nil, fmt.Errorf("compile core module %d of the component: %w", ci.module, cerr)
		}
		p.compiled[k] = cm
	}
	cm, ok := p.compiled[p.run.inst]
	if !ok {
		return nil, fmt.Errorf("the component's run function is not exported by a core module")
	}
	def, ok := cm.ExportedFunctions()[p.run.name]
	if !ok || len(def.ParamTypes()) != 0 || len(def.ResultTypes()) != 1 || def.ResultTypes()[0] != api.ValueTypeI32 {
		return nil, fmt.Errorf("the component's wasi:cli/run function has an unexpected core signature")
	}
	return p, nil
}

// checkMemories refuses a component that would create more than one linear
// memory. The runner's memory ceiling applies to each memory, so it holds for
// the whole program only when there is one.
func checkMemories(s *space) {
	memories := 0
	for _, ci := range s.coreInstances {
		if ci.real {
			memories += s.coreModules[ci.module].memories
		}
	}
	if memories > 1 {
		fail("the component defines %d linear memories; TAP runs components with one, so its memory ceiling holds", memories)
	}
}

// findRun returns the lifted function exported as wasi:cli/run's run.
func findRun(s *space) *compFunc {
	for name, v := range s.exports {
		base, version, _ := strings.Cut(name, "@")
		if base != "wasi:cli/run" {
			continue
		}
		if !strings.HasPrefix(version, "0.2.") {
			fail("the component exports %s; this runner runs WASI 0.2 commands", name)
		}
		inst, ok := v.(*compInstance)
		if !ok || inst.exports == nil {
			fail("the component's %s export is not an instance", name)
		}
		f, ok := inst.exports["run"].(*compFunc)
		if !ok || f.lifted == nil {
			fail("the component's %s export has no run function", name)
		}
		return f
	}
	fail("the component does not export wasi:cli/run; build it as a command (a program with a main function)")
	return nil
}

func instName(k int) string { return "c" + strconv.Itoa(k) }
func hostName(k int) string { return "h" + strconv.Itoa(k) }

// Run instantiates the component's core modules in order and calls its run
// function. A guest exit with status 0 and a run that returns ok both return
// nil; failure is a *sys.ExitError, as for a WASI Preview 1 module.
func (p *Program) Run(ctx context.Context, cfg Config) (err error) {
	st := newState(cfg)
	for k, ci := range p.space.coreInstances {
		if !ci.real {
			continue
		}
		if len(ci.hosts) > 0 {
			hb := p.rt.NewHostModuleBuilder(hostName(k))
			for j, h := range ci.hosts {
				hb.NewFunctionBuilder().
					WithGoModuleFunction(st.hostFunction(h, ci.sigs[j]), ci.sigs[j].params, ci.sigs[j].results).
					Export("f" + strconv.Itoa(j))
			}
			if _, err := hb.Instantiate(ctx); err != nil {
				return err
			}
		}
		mod, err := p.rt.InstantiateModule(ctx, p.compiled[k], wazero.NewModuleConfig().WithName(instName(k)).WithStartFunctions())
		if err != nil {
			return exitResult(err)
		}
		st.insts[k] = mod
	}
	fn := st.insts[p.run.inst].ExportedFunction(p.run.name)
	res, err := fn.Call(ctx)
	if err != nil {
		return exitResult(err)
	}
	if p.post != nil {
		if pr := st.insts[p.post.inst].ExportedFunction(p.post.name); pr != nil {
			if _, err := pr.Call(ctx, res...); err != nil {
				return exitResult(err)
			}
		}
	}
	if res[0] != 0 {
		return sys.NewExitError(1)
	}
	return nil
}

// exitResult treats an exit with status 0 as success, like wazero does for a
// WASI Preview 1 module's _start.
func exitResult(err error) error {
	var ee *sys.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 0 {
		return nil
	}
	return err
}
