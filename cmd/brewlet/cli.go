package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, ",") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

// newFlagSet returns a flag set whose -h/--help output is the command synopsis
// followed by its flags, written to out. Parse returns flag.ErrHelp in that
// case, which main treats as success.
func newFlagSet(name, synopsis string, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprintln(out, strings.TrimRight(synopsis, "\n"))
		var any bool
		fs.VisitAll(func(*flag.Flag) { any = true })
		if any {
			fmt.Fprintln(out, "\nFlags:")
			fs.PrintDefaults()
		}
	}
	return fs
}

// parseRefCommand parses flags for a command taking exactly one positional
// REF, accepting both `verb REF --flags` and `verb --flags REF`.
func parseRefCommand(fs *flag.FlagSet, args []string) (string, error) {
	ref := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		ref, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if ref == "" && fs.NArg() == 1 {
		ref = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return "", fmt.Errorf("%s: unexpected arguments %q", fs.Name(), fs.Args())
	}
	if ref == "" {
		return "", fmt.Errorf("%s: explicit reference required (see brewlet %s --help)", fs.Name(), fs.Name())
	}
	return ref, nil
}

func isHelp(err error) bool { return errors.Is(err, flag.ErrHelp) }
