package debugger

import "fmt"

type LaunchOptions struct {
	Cwd string
}

type optionsLauncher interface {
	LaunchWithOptions(string, []string, []string, LaunchOptions) error
}

// LaunchWithOptions keeps existing Debugger implementations source-compatible.
// An implementation without cwd support must reject it rather than silently
// run the target in a different directory.
func LaunchWithOptions(d Debugger, program string, args, env []string, options LaunchOptions) error {
	if options.Cwd == "" {
		return d.Launch(program, args, env)
	}
	launcher, ok := d.(optionsLauncher)
	if !ok {
		return fmt.Errorf("launch: debugger does not support cwd")
	}
	return launcher.LaunchWithOptions(program, args, env, options)
}
