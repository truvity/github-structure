package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"

	"github.com/truvity/github-structure/pkg/staterename"
)

const renameRepoUsage = "usage: state rename-repo <old> <new> [--apply] [--backup-dir <dir>]" +
	" (the backup goes to the user cache directory unless --backup-dir says otherwise)"

// state runs the state subcommands. The only one is rename-repo.
func (r *runner) state(ctx context.Context, org string, args []string) error {
	if len(args) == 0 || args[0] != "rename-repo" {
		return usageError(renameRepoUsage)
	}

	return r.renameRepo(ctx, org, args[1:])
}

// renameRepo re-keys the org stack's state after a repository was renamed
// on GitHub and its registry row was renamed. It is a dry run unless
// --apply is given; with --apply it saves the state it read before it
// writes the new one. The transform and its refusals are
// staterename.Rekey's.
func (r *runner) renameRepo(ctx context.Context, org string, args []string) error {
	oldName, newName, apply, backupDir, err := renameRepoArgs(args)
	if err != nil {
		return err
	}

	if r.cfg.Stack == nil {
		return errors.New("state rename-repo is not configured: Config.Stack is nil")
	}

	reg, _, err := r.lookup(org)
	if err != nil {
		return err
	}

	stack, err := r.cfg.Stack(ctx, org)
	if err != nil {
		return fmt.Errorf("select stack %s: %w", org, err)
	}

	exported, err := r.chk.exportState(ctx, stack)
	if err != nil {
		return fmt.Errorf("export state of %s: %w", org, err)
	}

	rekeyed, rep, err := staterename.Rekey(exported.Deployment, org, reg, oldName, newName)
	if err != nil {
		return err
	}

	if rep.Noop {
		_, _ = fmt.Fprintf(r.stdout(), "%s: the state already holds %q and nothing named %q; nothing to do\n", org, newName, oldName)

		return nil
	}

	r.printRekey(org, rep, apply)

	if !apply {
		_, _ = fmt.Fprintf(r.stdout(), "dry run: nothing was written. Rename the repository on GitHub first if that is not done, then repeat with --apply.\n")

		return nil
	}

	backup, err := r.saveBackup(org, oldName, newName, backupDir, exported)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(r.stdout(), "backup of the state before the change: %s\n", backup)

	if err := r.chk.importState(ctx, stack, apitype.UntypedDeployment{Version: exported.Version, Deployment: rekeyed}); err != nil {
		return fmt.Errorf("import the re-keyed state of %s (the unchanged state is in %s): %w", org, backup, err)
	}

	_, _ = fmt.Fprintf(r.stdout(), "state re-keyed. Verify with a refresh preview: it must plan no create, delete, replace or update.\n"+
		"To roll back: %s --org %s state rename-repo %s %s --apply, after reverting the registry row.\n",
		r.name(), org, newName, oldName)

	return nil
}

func (r *runner) printRekey(org string, rep staterename.Report, apply bool) {
	verb := "would re-key"
	if apply {
		verb = "re-keying"
	}

	_, _ = fmt.Fprintf(r.stdout(), "%s: %s %q to %q: %d resource(s) move, %d reference(s) follow\n",
		org, verb, rep.From, rep.To, len(rep.Moved), rep.References)

	for _, m := range rep.Moved {
		_, _ = fmt.Fprintf(r.stdout(), "  %s -> %s\n", resourceName(m.From), resourceName(m.To))
	}
}

func resourceName(urn string) string {
	return urn[strings.LastIndex(urn, "::")+2:]
}

// saveBackup writes the exported state, as `pulumi stack import --file`
// reads it, to a new file that nothing else may read: a state holds
// encrypted secrets and provider ids.
func (r *runner) saveBackup(org, oldName, newName, dir string, exported apitype.UntypedDeployment) (string, error) {
	raw, err := json.MarshalIndent(exported, "", "    ")
	if err != nil {
		return "", fmt.Errorf("encode backup: %w", err)
	}

	dir, err = r.backupDirectory(dir)
	if err != nil {
		return "", fmt.Errorf("save backup (nothing was changed): %w", err)
	}

	name := fmt.Sprintf("%s-%s-state-before-rename-%s-to-%s-%s.json",
		r.name(), org, oldName, newName, r.chk.now().UTC().Format("20060102T150405Z"))
	path := filepath.Join(dir, name)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("save backup (nothing was changed): %w", err)
	}

	if _, err := f.Write(append(raw, '\n')); err != nil {
		_ = f.Close()

		return "", fmt.Errorf("save backup (nothing was changed): %w", err)
	}

	if err := f.Close(); err != nil {
		return "", fmt.Errorf("save backup (nothing was changed): %w", err)
	}

	return path, nil
}

// backupDirectory is where the backup goes. Unset, that is a private
// directory under the user's cache directory (the OS temp directory where
// there is none): never the working directory, which is often a checkout
// whose files get committed. A directory the operator names that sits
// inside a git work tree is used, with a warning.
func (r *runner) backupDirectory(dir string) (string, error) {
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil || base == "" {
			base = os.TempDir()
		}

		dir = filepath.Join(base, r.name())
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	if abs, err := filepath.Abs(dir); err == nil && insideGitWorkTree(abs) {
		_, _ = fmt.Fprintf(r.stderr(), "warning: the backup directory %s is inside a git work tree; "+
			"the backup holds the whole state (encrypted secrets, provider ids), so do not commit it\n", abs)
	}

	return dir, nil
}

// insideGitWorkTree reports whether dir or a parent holds a .git entry.
func insideGitWorkTree(dir string) bool {
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return true
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}

		dir = parent
	}
}

// renameRepoArgs parses `<old> <new> [--apply] [--backup-dir <dir>]`, flags
// in any position.
func renameRepoArgs(args []string) (oldName, newName string, apply bool, backupDir string, err error) {
	fset := flag.NewFlagSet("state rename-repo", flag.ContinueOnError)
	fset.SetOutput(io.Discard)
	fset.BoolVar(&apply, "apply", false, "write the re-keyed state (default: dry run)")
	fset.StringVar(&backupDir, "backup-dir", "", "where --apply saves the state it read (default: <user cache dir>/<tool name>)")

	var pos []string

	for rest := args; ; {
		if perr := fset.Parse(rest); perr != nil {
			return "", "", false, "", usageError(renameRepoUsage + ": " + perr.Error())
		}

		rest = fset.Args()
		if len(rest) == 0 {
			break
		}

		pos = append(pos, rest[0])
		rest = rest[1:]
	}

	if len(pos) != 2 {
		return "", "", false, "", usageError(renameRepoUsage)
	}

	return pos[0], pos[1], apply, backupDir, nil
}
