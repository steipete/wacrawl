package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/openclaw/wacrawl/internal/backup"
	"github.com/openclaw/wacrawl/internal/store"
)

func (a *app) runBackup(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printCommandUsage(a.stdout, "backup")
		return nil
	}
	if args[0] == "help" {
		if len(args) == 1 {
			printCommandUsage(a.stdout, "backup")
			return nil
		}
		if printCommandUsage(a.stdout, append([]string{"backup"}, args[1:]...)...) {
			return nil
		}
		return usageErr(fmt.Errorf("unknown backup help topic %q", strings.Join(args[1:], " ")))
	}
	switch args[0] {
	case "init":
		return a.runBackupInit(ctx, args[1:])
	case "push":
		return a.runBackupPush(ctx, args[1:])
	case "pull":
		return a.runBackupPull(ctx, args[1:])
	case "status":
		return a.runBackupStatus(ctx, args[1:])
	case "snapshots":
		return a.runBackupSnapshots(ctx, args[1:])
	default:
		return usageErr(fmt.Errorf("unknown backup command %q", args[0]))
	}
}

func (a *app) runBackupInit(ctx context.Context, args []string) error {
	fs, opts, noPush := backupFlagSet("backup init")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printCommandUsage(a.stdout, "backup", "init")
			return nil
		}
		return usageErr(err)
	}
	if fs.NArg() != 0 {
		return usageErr(errors.New("backup init takes flags only"))
	}
	opts.Push = !*noPush
	opts.ArchivePath, opts.SourcePath = a.dbPath, a.source
	cfg, recipient, err := backup.Init(ctx, *opts)
	if err != nil {
		return err
	}
	if a.json {
		return a.print(map[string]any{"repo": cfg.Repo, "remote": cfg.Remote, "identity": cfg.Identity, "recipient": recipient})
	}
	_, err = fmt.Fprintf(a.stdout, "repo=%s\nremote=%s\nidentity=%s\nrecipient=%s\n", cfg.Repo, cfg.Remote, cfg.Identity, recipient)
	return err
}

func (a *app) runBackupPush(ctx context.Context, args []string) error {
	fs, opts, noPush := backupFlagSet("backup push")
	fs.StringVar(&opts.Tag, "tag", "", "")
	fs.BoolVar(&opts.NoMedia, "no-media", false, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printCommandUsage(a.stdout, "backup", "push")
			return nil
		}
		return usageErr(err)
	}
	if fs.NArg() != 0 {
		return usageErr(errors.New("backup push takes flags only"))
	}
	opts.Push = !*noPush
	opts.ArchivePath, opts.SourcePath = a.dbPath, a.source
	if err := backup.ValidateWriteOptions(*opts); err != nil {
		return err
	}
	return a.withArchiveStore(ctx, func(st *store.Store) error {
		result, err := backup.Push(ctx, st, *opts)
		if err != nil {
			return err
		}
		return a.print(result)
	})
}

func (a *app) runBackupPull(ctx context.Context, args []string) error {
	fs, opts, _ := backupFlagSet("backup pull")
	fs.StringVar(&opts.Ref, "ref", "", "")
	fs.BoolVar(&opts.NoMedia, "no-media", false, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printCommandUsage(a.stdout, "backup", "pull")
			return nil
		}
		return usageErr(err)
	}
	if fs.NArg() != 0 {
		return usageErr(errors.New("backup pull takes flags only"))
	}
	return a.withStore(ctx, func(st *store.Store) error {
		result, err := backup.Pull(ctx, st, *opts)
		if err != nil {
			return err
		}
		return a.print(result)
	})
}

func (a *app) runBackupSnapshots(ctx context.Context, args []string) error {
	fs, opts, _ := backupFlagSet("backup snapshots")
	fs.IntVar(&opts.Limit, "limit", 20, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printCommandUsage(a.stdout, "backup", "snapshots")
			return nil
		}
		return usageErr(err)
	}
	if fs.NArg() != 0 {
		return usageErr(errors.New("backup snapshots takes flags only"))
	}
	if opts.Limit < 1 {
		return usageErr(errors.New("backup snapshots --limit must be greater than zero"))
	}
	snapshots, repo, err := backup.Snapshots(ctx, *opts)
	if err != nil {
		return err
	}
	if a.json {
		return a.print(map[string]any{"repo": repo, "snapshots": snapshots})
	}
	return a.print(snapshots)
}

func (a *app) runBackupStatus(ctx context.Context, args []string) error {
	fs, opts, _ := backupFlagSet("backup status")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printCommandUsage(a.stdout, "backup", "status")
			return nil
		}
		return usageErr(err)
	}
	if fs.NArg() != 0 {
		return usageErr(errors.New("backup status takes flags only"))
	}
	manifest, repo, err := backup.Status(ctx, *opts)
	if err != nil {
		return err
	}
	if a.json {
		return a.print(map[string]any{"repo": repo, "manifest": manifest})
	}
	if err := a.print(manifest); err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.stdout, "repo=%s\n", repo)
	return err
}

func backupFlagSet(name string) (*flag.FlagSet, *backup.Options, *bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	opts := &backup.Options{}
	fs.StringVar(&opts.ConfigPath, "config", backup.DefaultConfigPath(), "")
	fs.StringVar(&opts.Repo, "repo", "", "")
	fs.StringVar(&opts.Remote, "remote", "", "")
	fs.StringVar(&opts.Identity, "identity", "", "")
	fs.Func("recipient", "", func(value string) error {
		opts.Recipients = append(opts.Recipients, value)
		return nil
	})
	noPush := fs.Bool("no-push", false, "")
	return fs, opts, noPush
}
