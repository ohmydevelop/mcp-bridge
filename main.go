package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"newmcp/mcp-bridge/internal/bridge"
	"newmcp/mcp-bridge/internal/cliadapter"
	"newmcp/mcp-bridge/internal/localipc"
	"newmcp/mcp-bridge/internal/mcpapp"
	"newmcp/mcp-bridge/internal/platform"
	"newmcp/mcp-bridge/internal/pyrunner"
	"newmcp/mcp-bridge/internal/terminal"
)

func main() {
	if handled, code := platform.ProcessHelper(); handled {
		os.Exit(code)
	}
	if e := run(); e != nil {
		var exit *exec.ExitError
		if errors.As(e, &exit) {
			code := exit.ExitCode()
			if code < 0 {
				code = 128 + int(exit.Sys().(syscall.WaitStatus).Signal())
			}
			os.Exit(code)
		}
		log.Print(e)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: mcp-bridge serve|stdio|cli|run|doctor|python-path [-data DIR] [-listen 127.0.0.1:8088]")
	}
	if os.Args[1] == "cli" {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		return cliadapter.Serve(ctx, os.Args[2:])
	}
	if os.Args[1] == "python-path" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		path, err := pyrunner.PythonPath(ctx)
		if err == nil {
			fmt.Println(path)
		}
		return err
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ContinueOnError)
	const defaultDir = "~/.mcp-bridge"
	dir := fs.String("data", defaultDir, "private state directory")
	listen := fs.String("listen", "127.0.0.1:8088", "HTTP address (TLS terminating proxy for remote clients)")
	var delivery string
	if os.Args[1] == "run" {
		fs.StringVar(&delivery, "delivery", "inbox", "inbox or auto")
	}
	var app, scopePerProcess bool
	if os.Args[1] == "serve" {
		fs.BoolVar(&app, "app", false, "enable the management MCP App")
	}
	if os.Args[1] == "stdio" {
		fs.BoolVar(&scopePerProcess, "one-task-per-process", false, "use a fresh scope for a stdio process dedicated to one task")
	}
	if e := fs.Parse(os.Args[2:]); e != nil {
		return e
	}
	stateDir := *dir
	if stateDir == defaultDir {
		home, err := os.UserHomeDir()
		if err != nil {
			return errors.New("cannot resolve home directory; specify -data DIR")
		}
		stateDir = filepath.Join(home, ".mcp-bridge")
	}
	abs, e := filepath.Abs(stateDir)
	if e != nil {
		return e
	}
	if os.Args[1] == "doctor" {
		return bridge.CheckLocal(abs, os.Stdout)
	}
	if os.Args[1] == "run" {
		return terminal.Run(abs, delivery, fs.Args())
	}
	if os.Args[1] == "stdio" {
		c, e := localipc.Dial(abs)
		if e != nil {
			return e
		}
		defer c.Close()
		if token := os.Getenv(terminal.Env); token != "" {
			if len(token) != 64 {
				return errors.New("invalid terminal binding")
			}
			if _, e = fmt.Fprintf(c, "B%s\n", token); e != nil {
				return e
			}
		}
		// A long-lived client can multiplex unrelated tasks on a single MCP connection.
		// Only infer a scope when its launcher explicitly declares a task boundary.
		scope := os.Getenv("BRIDGE_TASK_SCOPE")
		if scopePerProcess && scope != "" {
			return errors.New("choose BRIDGE_TASK_SCOPE or -one-task-per-process")
		}
		if scopePerProcess {
			var nonce [16]byte
			rand.Read(nonce[:])
			scope = hex.EncodeToString(nonce[:])
		}
		if scope != "" && !bridge.ValidTaskScope(scope) {
			return errors.New("invalid BRIDGE_TASK_SCOPE")
		}
		go func() {
			defer c.CloseWrite()
			if scope == "" {
				io.Copy(c, os.Stdin)
				return
			}
			if err := forwardScoped(os.Stdin, c, scope); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}()
		_, e = io.Copy(os.Stdout, c)
		return e
	}
	if os.Args[1] != "serve" {
		return errors.New("unknown command")
	}
	var registerApp func(*mcp.Server)
	if app {
		registerApp = mcpapp.Register
	}
	return bridge.Serve(abs, *listen, registerApp)
}
