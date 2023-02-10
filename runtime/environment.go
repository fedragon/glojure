package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/glojurelang/glojure/value"
)

var (
	SymbolUnquote       = value.NewSymbol("clojure.core/unquote") // TODO: rename to glojure.core/unquote
	SymbolSpliceUnquote = value.NewSymbol("splice-unquote")
	SymbolNamespace     = value.NewSymbol("ns")
	SymbolInNamespace   = value.NewSymbol("in-ns")
	SymbolUserNamespace = value.NewSymbol("user")
	SymbolDot           = value.NewSymbol(".")
)

type (
	environment struct {
		ctx context.Context

		// local bindings
		scope *scope

		recurTarget interface{}

		currentNamespaceVar *value.Var
		namespaces          map[string]*value.Namespace
		nsMtx               *sync.RWMutex

		// some well-known vars
		namespaceVar   *value.Var // ns
		inNamespaceVar *value.Var // in-ns

		// counter for gensym (symbol generator)
		symCounter int32

		stdout io.Writer
		stderr io.Writer

		loadPath []string
	}
)

func newEnvironment(ctx context.Context, stdout, stderr io.Writer) *environment {
	e := &environment{
		ctx:        ctx,
		scope:      newScope(),
		namespaces: make(map[string]*value.Namespace),
		nsMtx:      &sync.RWMutex{},
		stdout:     stdout,
		stderr:     stderr,
	}
	coreNS := e.FindOrCreateNamespace(value.SymbolCoreNamespace)
	e.currentNamespaceVar = value.NewVarWithRoot(coreNS, value.NewSymbol("*ns*"), coreNS)
	coreNS.InternWithValue(e, value.NewSymbol("*agent*"), nil, true)

	// bootstrap some vars
	e.namespaceVar = value.NewVarWithRoot(coreNS, SymbolNamespace,
		value.ApplyerFunc(func(env value.Environment, args []interface{}) (interface{}, error) { return coreNS, nil }))
	e.namespaceVar.SetMacro()

	e.inNamespaceVar = value.NewVarWithRoot(coreNS, SymbolInNamespace, false)

	return e
}

func (env *environment) nextSymNum() int32 {
	for {
		val := atomic.LoadInt32(&env.symCounter)
		if atomic.CompareAndSwapInt32(&env.symCounter, val, val+1) {
			return val
		}
	}
}

func (env *environment) Context() context.Context {
	return env.ctx
}

func (env *environment) String() string {
	return fmt.Sprintf("object[Environment]")
}

// TODO: rename to something else; this isn't for `def`s, it's for
// local bindings.
func (env *environment) BindLocal(sym *value.Symbol, val interface{}) {
	env.scope.define(sym, val)
}

func (env *environment) DefVar(sym *value.Symbol, val interface{}) *value.Var {
	// TODO: match clojure implementation more closely
	v := env.CurrentNamespace().InternWithValue(env, sym, val, true /* replace root */)
	if meta := sym.Meta(); meta != nil {
		v.SetMeta(meta)
	}
	return v
}

func (env *environment) DefineMacro(name string, fn value.Applyer) {
	vr := env.DefVar(value.NewSymbol(name), fn)
	vr.SetMacro()
}

func (env *environment) lookup(sym *value.Symbol) (interface{}, bool) {
	v, ok := env.scope.lookup(sym)
	if ok {
		return v, true
	}

	{ // HACKHACK
		// TODO: implement *ns* as a normal var
		if sym.String() == "*ns*" {
			return env.CurrentNamespace(), true
		}
	}

	ns := env.CurrentNamespace()
	if sym.Namespace() != "" {
		ns = env.FindNamespace(value.NewSymbol(sym.Namespace()))
		sym = value.NewSymbol(sym.Name())
	}
	if ns == nil {
		return nil, false
	}
	vr, ok := ns.Mappings().ValueAt(sym)
	if !ok {
		return nil, false
	}
	// TODO: can these only be vars?
	return vr.(*value.Var).Get(), true
}

func (env *environment) WithRecurTarget(rt interface{}) value.Environment {
	wrappedEnv := *env
	newEnv := &wrappedEnv
	newEnv.recurTarget = rt
	return newEnv
}

func (env *environment) PushScope() value.Environment {
	wrappedEnv := *env
	newEnv := &wrappedEnv
	newEnv.scope = newEnv.scope.push()
	return newEnv
}

func (env *environment) Stdout() io.Writer {
	return env.stdout
}

func (env *environment) Stderr() io.Writer {
	return env.stderr
}

func (env *environment) FindNamespace(sym *value.Symbol) *value.Namespace {
	env.nsMtx.RLock()
	defer env.nsMtx.RUnlock()
	return env.namespaces[sym.String()]
}

func (env *environment) FindOrCreateNamespace(sym *value.Symbol) *value.Namespace {
	ns := env.FindNamespace(sym)
	if ns != nil {
		return ns
	}
	env.nsMtx.Lock()
	defer env.nsMtx.Unlock()
	ns = env.namespaces[sym.String()]
	if ns != nil {
		return ns
	}
	ns = value.NewNamespace(sym)
	env.namespaces[sym.String()] = ns
	return ns
}

func (env *environment) CurrentNamespace() *value.Namespace {
	return env.currentNamespaceVar.Get().(*value.Namespace)
}

func (env *environment) SetCurrentNamespace(ns *value.Namespace) {
	env.currentNamespaceVar.BindRoot(ns)
}

func (env *environment) PushLoadPaths(paths []string) value.Environment {
	newEnv := &(*env)
	newEnv.loadPath = append(paths, newEnv.loadPath...)
	return newEnv
}

func (env *environment) ResolveFile(filename string) (string, bool) {
	if filepath.IsAbs(filename) {
		return filename, true
	}

	for _, path := range env.loadPath {
		fullPath := filepath.Join(path, filename)
		if _, err := os.Stat(fullPath); err == nil {
			return fullPath, true
		}
	}
	return "", false
}

func (env *environment) Errorf(n interface{}, format string, args ...interface{}) error {
	return env.errorf(n, format, args...)
}

func (env *environment) errorf(n interface{}, format string, args ...interface{}) error {
	var filename, line, col string
	var meta value.IPersistentMap
	if n, ok := n.(value.IObj); ok {
		meta = n.Meta()
	}
	get := func(m value.IPersistentMap, key string) string {
		return value.ToString(value.GetDefault(m, value.NewKeyword(key), "?"), value.PrintReadably())
	}

	filename = get(meta, "file")
	line = get(meta, "line")
	col = get(meta, "column")

	location := fmt.Sprintf("%s:%s:%s", filename, line, col)

	return fmt.Errorf("%s: "+format, append([]interface{}{location}, args...)...)
}
