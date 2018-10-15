package skycfg

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/golang/protobuf/proto"
	"github.com/google/skylark"
	"github.com/google/skylark/skylarkstruct"

	impl "github.com/stripe/skycfg/internal/go/skycfg"
)

type FileReader interface {
	Resolve(ctx context.Context, name, fromPath string) (path string, err error)
	ReadFile(ctx context.Context, path string) ([]byte, error)
}

type localFileReader struct {
	root string
}

func LocalFileReader(root string) FileReader {
	if root == "" {
		panic("LocalFileReader: empty root path")
	}
	return &localFileReader{root}
}

func (r *localFileReader) Resolve(ctx context.Context, name, fromPath string) (string, error) {
	if fromPath == "" {
		return name, nil
	}
	if filepath.Separator != '/' && strings.ContainsRune(name, filepath.Separator) {
		return "", fmt.Errorf("load(%q): invalid character in module name", name)
	}
	resolved := filepath.Join(r.root, filepath.FromSlash(path.Clean("/"+name)))
	return resolved, nil
}

func (r *localFileReader) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return ioutil.ReadFile(path)
}

func NewProtoMessage(msg proto.Message) skylark.Value {
	return impl.NewSkyProtoMessage(msg)
}

func AsProtoMessage(v skylark.Value) (proto.Message, bool) {
	return impl.ToProtoMessage(v)
}

type Config struct {
	filename string
	globals  skylark.StringDict
	locals   skylark.StringDict
}

type LoadOption interface {
	applyLoad(*loadOptions)
}

type loadOptions struct {
	globals       skylark.StringDict
	fileReader    FileReader
	protoRegistry impl.ProtoRegistry
}

type fnLoadOption func(*loadOptions)

func (fn fnLoadOption) applyLoad(opts *loadOptions) { fn(opts) }

type unstableProtoRegistry interface {
	impl.ProtoRegistry
}

func WithGlobals(globals skylark.StringDict) LoadOption {
	return fnLoadOption(func(opts *loadOptions) {
		for key, value := range globals {
			opts.globals[key] = value
		}
	})
}

func WithFileReader(r FileReader) LoadOption {
	if r == nil {
		panic("WithFileReader: nil reader")
	}
	return fnLoadOption(func(opts *loadOptions) {
		opts.fileReader = r
	})
}

func WithProtoRegistry(r unstableProtoRegistry) LoadOption {
	if r == nil {
		panic("WithProtoRegistry: nil registry")
	}
	return fnLoadOption(func(opts *loadOptions) {
		opts.protoRegistry = r
	})
}

func Load(ctx context.Context, filename string, opts ...LoadOption) (*Config, error) {
	protoModule := impl.NewProtoModule(nil /* TODO: registry from options */)
	parsedOpts := &loadOptions{
		globals: skylark.StringDict{
			"fail":   skylark.NewBuiltin("fail", skyFail),
			"hash":   impl.HashModule(),
			"json":   impl.JsonModule(),
			"proto":  protoModule,
			"struct": skylark.NewBuiltin("struct", skylarkstruct.Make),
			"yaml":   impl.YamlModule(),
			"url":    impl.UrlModule(),
		},
		fileReader: LocalFileReader(filepath.Dir(filename)),
	}
	for _, opt := range opts {
		opt.applyLoad(parsedOpts)
	}
	protoModule.Registry = parsedOpts.protoRegistry
	configLocals, err := loadImpl(ctx, parsedOpts, filename)
	if err != nil {
		return nil, err
	}
	return &Config{
		filename: filename,
		globals:  parsedOpts.globals,
		locals:   configLocals,
	}, nil
}

func loadImpl(ctx context.Context, opts *loadOptions, filename string) (skylark.StringDict, error) {
	reader := opts.fileReader

	type cacheEntry struct {
		globals skylark.StringDict
		err     error
	}
	cache := make(map[string]*cacheEntry)

	var load func(thread *skylark.Thread, moduleName string) (skylark.StringDict, error)
	load = func(thread *skylark.Thread, moduleName string) (skylark.StringDict, error) {
		var fromPath string
		if thread.TopFrame() != nil {
			fromPath = thread.TopFrame().Position().Filename()
		}
		modulePath, err := reader.Resolve(ctx, moduleName, fromPath)
		if err != nil {
			return nil, err
		}

		e, ok := cache[modulePath]
		if e != nil {
			return e.globals, e.err
		}
		if ok {
			return nil, fmt.Errorf("cycle in load graph")
		}
		moduleSource, err := reader.ReadFile(ctx, modulePath)
		if err != nil {
			cache[modulePath] = &cacheEntry{nil, err}
			return nil, err
		}

		cache[modulePath] = nil
		globals, err := skylark.ExecFile(thread, modulePath, moduleSource, opts.globals)
		cache[modulePath] = &cacheEntry{globals, err}
		return globals, err
	}
	return load(&skylark.Thread{
		Print: skyPrint,
		Load:  load,
	}, filename)
}

func (c *Config) Filename() string {
	return c.filename
}

func (c *Config) Globals() skylark.StringDict {
	return c.globals
}

func (c *Config) Locals() skylark.StringDict {
	return c.locals
}

type ExecOption interface {
	applyExec(*execOptions)
}

type execOptions struct {
	vars *skylark.Dict
}

type fnExecOption func(*execOptions)

func (fn fnExecOption) applyExec(opts *execOptions) { fn(opts) }

func WithVars(vars skylark.StringDict) ExecOption {
	return fnExecOption(func(opts *execOptions) {
		for key, value := range vars {
			opts.vars.Set(skylark.String(key), value)
		}
	})
}

func (c *Config) Main(ctx context.Context, opts ...ExecOption) ([]proto.Message, error) {
	parsedOpts := &execOptions{
		vars: &skylark.Dict{},
	}
	for _, opt := range opts {
		opt.applyExec(parsedOpts)
	}
	mainVal, ok := c.locals["main"]
	if !ok {
		return nil, fmt.Errorf("no `main' function found in %q", c.filename)
	}
	main, ok := mainVal.(skylark.Callable)
	if !ok {
		return nil, fmt.Errorf("`main' must be a function (got a %s)", mainVal.Type())
	}

	thread := &skylark.Thread{
		Print: skyPrint,
	}
	thread.SetLocal("context", ctx)
	mainCtx := &impl.Module{
		Name: "skycfg_ctx",
		Attrs: skylark.StringDict(map[string]skylark.Value{
			"vars": parsedOpts.vars,
		}),
	}
	args := skylark.Tuple([]skylark.Value{mainCtx})
	mainVal, err := main.Call(thread, args, nil)
	if err != nil {
		return nil, err
	}
	mainList, ok := mainVal.(*skylark.List)
	if !ok {
		if _, isNone := mainVal.(skylark.NoneType); isNone {
			return nil, nil
		}
		return nil, fmt.Errorf("`main' didn't return a list (got a %s)", mainVal.Type())
	}
	var msgs []proto.Message
	for ii := 0; ii < mainList.Len(); ii++ {
		maybeMsg := mainList.Index(ii)
		msg, ok := AsProtoMessage(maybeMsg)
		if !ok {
			return nil, fmt.Errorf("`main' returned something that's not a protobuf (a %s)", maybeMsg.Type())
		}
		msgs = append(msgs, msg)
	}
	return msgs, nil
}

func skyPrint(t *skylark.Thread, msg string) {
	fmt.Fprintf(os.Stderr, "[%v] %s\n", t.Caller().Position(), msg)
}

func skyFail(t *skylark.Thread, fn *skylark.Builtin, args skylark.Tuple, kwargs []skylark.Tuple) (skylark.Value, error) {
	var msg string
	if err := skylark.UnpackPositionalArgs(fn.Name(), args, kwargs, 1, &msg); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	t.Caller().WriteBacktrace(&buf)
	return nil, fmt.Errorf("[%s] %s\n%s", t.Caller().Position(), msg, buf.String())
}
