package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gosuda/flats/internal/backup"
	"github.com/gosuda/flats/internal/buildinfo"
)

// maxFailuresShown bounds the failures that `flats backup --verify` prints.
const maxFailuresShown = 100

// BackupCommand runs `flats backup --out` and `flats backup --verify` (see
// docs/spec/host-backup.md). Neither calls the host.
func BackupCommand(ctx context.Context, args []string, out io.Writer) error {
	b := &backupCmd{configCmd: configCmd{ctx: ctx, out: out}}
	f := b.flags("backup")
	outFlag := f.String("out", "", "the new folder to write the copy to")
	verify := f.String("verify", "", "check the copy in this folder")
	exclude := f.Bool("exclude-secret-key", false, "leave secret.key out of the copy")
	if err := b.parseFlags(args); err != nil {
		return err
	}
	switch {
	case *outFlag != "" && *verify != "":
		return usagef("--out and --verify cannot be used together")
	case *outFlag == "" && *verify == "":
		return usagef("say --out PATH to make a copy or --verify PATH to check one")
	case *verify != "" && (*exclude || b.configFlag != "" || b.dataFlag != ""):
		return usagef("--verify takes no other option")
	case *outFlag == "" && *exclude:
		return usagef("--exclude-secret-key goes with --out")
	}
	if *verify != "" {
		return b.verify(*verify)
	}
	return b.run(*outFlag, *exclude)
}

type backupCmd struct{ configCmd }

func (b *backupCmd) run(out string, exclude bool) error {
	configPath, _, err := b.paths()
	if err != nil {
		return err
	}
	dataDir, err := b.dataDir()
	if err != nil {
		return err
	}
	res, err := backup.Run(b.ctx, backup.Options{
		DataDir: dataDir, ConfigPath: configPath, Out: out,
		ExcludeSecretKey: exclude, FlatsVersion: buildinfo.Get().Version,
	})
	if err != nil {
		return backupError(b.ctx, err)
	}
	b.report(dataDir, res)
	return nil
}

// report prints what a finished backup lists (rule 24 of the spec).
func (b *backupCmd) report(dataDir string, res *backup.Result) {
	b.printf("backed up %s to %s\n", dataDir, res.Out)
	b.printf("  %d files, %d bytes\n", res.Files, res.Bytes)
	switch res.SecretKey {
	case backup.SecretKeyIncluded:
		b.printf("  secret.key is in the copy\n")
	case backup.SecretKeyExcluded:
		b.printf("  secret.key is not in the copy; the recovery copy %s stays, so `flats secret restore-key` can bring the key back\n", "secret.key.recovery")
	default:
		b.printf("  the host has no secret.key\n")
	}
	b.printf("  %d retries, %d attempt(s) at coherence\n", res.Retries, res.Attempts)
	for _, s := range res.Skipped {
		b.printf("  skipped %s: %s\n", s.Path, s.Reason)
	}
	for _, p := range res.Unstable {
		b.printf("  %s changed while it was copied; the copy holds its last read\n", p)
	}
	b.printf("The copy holds the secrets of this host. Keep it as protected as the data directory, in a folder that only you can write.\n")
	b.printf("Check it with `flats backup --verify %s` before you restore from it.\n", res.Out)
}

func (b *backupCmd) verify(dir string) error {
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return actionf("%s is not a folder", root)
	}
	res, err := backup.Verify(b.ctx, root)
	if err != nil {
		return backupError(b.ctx, err)
	}
	for i, f := range res.Failures {
		if i == maxFailuresShown {
			b.printf("... and %d more\n", len(res.Failures)-maxFailuresShown)
			break
		}
		b.printf("%s: %s\n", f.Path, f.Reason)
	}
	for _, n := range res.Notes {
		b.printf("note: %s\n", n)
	}
	if len(res.Failures) > 0 {
		return fmt.Errorf("%d checks failed for %s", len(res.Failures), root)
	}
	b.printf("ok: %s was made by Flats %s on %s\n", root, res.Manifest.FlatsVersion, res.Manifest.Created)
	return nil
}

// backupError maps what the backup package returns to an exit status: a refusal
// is 78, an interrupt names that nothing was written, and anything else is 1.
func backupError(ctx context.Context, err error) error {
	var refused *backup.RefusedError
	switch {
	case errors.As(err, &refused):
		return &ActionError{err}
	case ctx.Err() != nil:
		return interrupted(ctx.Err())
	}
	return err
}
