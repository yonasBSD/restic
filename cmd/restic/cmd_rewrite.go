package main

import (
	"context"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/sync/errgroup"

	"github.com/restic/restic/internal/debug"
	"github.com/restic/restic/internal/errors"
	"github.com/restic/restic/internal/filter"
	"github.com/restic/restic/internal/repository"
	"github.com/restic/restic/internal/restic"
	"github.com/restic/restic/internal/walker"
)

func newRewriteCommand() *cobra.Command {
	var opts RewriteOptions

	cmd := &cobra.Command{
		Use:   "rewrite [flags] [snapshotID ...]",
		Short: "Rewrite snapshots to exclude unwanted files",
		Long: `
The "rewrite" command excludes files from existing snapshots. It creates new
snapshots containing the same data as the original ones, but without the files
you specify to exclude. All metadata (time, host, tags) will be preserved.

The snapshots to rewrite are specified using the --host, --tag and --path options,
or by providing a list of snapshot IDs. Please note that specifying neither any of
these options nor a snapshot ID will cause the command to rewrite all snapshots.

The special tag 'rewrite' will be added to the new snapshots to distinguish
them from the original ones, unless --forget is used. If the --forget option is
used, the original snapshots will instead be directly removed from the repository.

Please note that the --forget option only removes the snapshots and not the actual
data stored in the repository. In order to delete the no longer referenced data,
use the "prune" command.

When rewrite is used with the --snapshot-summary option, a new snapshot is
created containing statistics summary data. Only two fields in the summary will
be non-zero: TotalFilesProcessed and TotalBytesProcessed.

When rewrite is called with one of the --exclude options, TotalFilesProcessed
and TotalBytesProcessed will be updated in the snapshot summary.

EXIT STATUS
===========

Exit status is 0 if the command was successful.
Exit status is 1 if there was any error.
Exit status is 10 if the repository does not exist.
Exit status is 11 if the repository is already locked.
Exit status is 12 if the password is incorrect.
`,
		GroupID:           cmdGroupDefault,
		DisableAutoGenTag: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRewrite(cmd.Context(), opts, globalOptions, args)
		},
	}

	opts.AddFlags(cmd.Flags())
	return cmd
}

type snapshotMetadata struct {
	Hostname string
	Time     *time.Time
}

type snapshotMetadataArgs struct {
	Hostname string
	Time     string
}

func (sma snapshotMetadataArgs) empty() bool {
	return sma.Hostname == "" && sma.Time == ""
}

func (sma snapshotMetadataArgs) convert() (*snapshotMetadata, error) {
	if sma.empty() {
		return nil, nil
	}

	var timeStamp *time.Time
	if sma.Time != "" {
		t, err := time.ParseInLocation(TimeFormat, sma.Time, time.Local)
		if err != nil {
			return nil, errors.Fatalf("error in time option: %v\n", err)
		}
		timeStamp = &t
	}
	return &snapshotMetadata{Hostname: sma.Hostname, Time: timeStamp}, nil
}

// RewriteOptions collects all options for the rewrite command.
type RewriteOptions struct {
	Forget          bool
	DryRun          bool
	SnapshotSummary bool

	Metadata snapshotMetadataArgs
	restic.SnapshotFilter
	filter.ExcludePatternOptions
}

func (opts *RewriteOptions) AddFlags(f *pflag.FlagSet) {
	f.BoolVarP(&opts.Forget, "forget", "", false, "remove original snapshots after creating new ones")
	f.BoolVarP(&opts.DryRun, "dry-run", "n", false, "do not do anything, just print what would be done")
	f.StringVar(&opts.Metadata.Hostname, "new-host", "", "replace hostname")
	f.StringVar(&opts.Metadata.Time, "new-time", "", "replace time of the backup")
	f.BoolVarP(&opts.SnapshotSummary, "snapshot-summary", "s", false, "create snapshot summary record if it does not exist")

	initMultiSnapshotFilter(f, &opts.SnapshotFilter, true)
	opts.ExcludePatternOptions.Add(f)
}

// rewriteFilterFunc returns the filtered tree ID or an error. If a snapshot summary is returned, the snapshot will
// be updated accordingly.
type rewriteFilterFunc func(ctx context.Context, sn *restic.Snapshot) (restic.ID, *restic.SnapshotSummary, error)

func rewriteSnapshot(ctx context.Context, repo *repository.Repository, sn *restic.Snapshot, opts RewriteOptions) (bool, error) {
	if sn.Tree == nil {
		return false, errors.Errorf("snapshot %v has nil tree", sn.ID().Str())
	}

	rejectByNameFuncs, err := opts.ExcludePatternOptions.CollectPatterns(Warnf)
	if err != nil {
		return false, err
	}

	metadata, err := opts.Metadata.convert()

	if err != nil {
		return false, err
	}

	var filter rewriteFilterFunc

	if len(rejectByNameFuncs) > 0 || opts.SnapshotSummary {
		selectByName := func(nodepath string) bool {
			for _, reject := range rejectByNameFuncs {
				if reject(nodepath) {
					return false
				}
			}
			return true
		}

		rewriteNode := func(node *restic.Node, path string) *restic.Node {
			if selectByName(path) {
				return node
			}
			Verbosef("excluding %s\n", path)
			return nil
		}

		rewriter, querySize := walker.NewSnapshotSizeRewriter(rewriteNode)

		filter = func(ctx context.Context, sn *restic.Snapshot) (restic.ID, *restic.SnapshotSummary, error) {
			id, err := rewriter.RewriteTree(ctx, repo, "/", *sn.Tree)
			if err != nil {
				return restic.ID{}, nil, err
			}
			ss := querySize()
			summary := &restic.SnapshotSummary{}
			if sn.Summary != nil {
				*summary = *sn.Summary
			}
			summary.TotalFilesProcessed = ss.FileCount
			summary.TotalBytesProcessed = ss.FileSize
			return id, summary, err
		}

	} else {
		filter = func(_ context.Context, sn *restic.Snapshot) (restic.ID, *restic.SnapshotSummary, error) {
			return *sn.Tree, nil, nil
		}
	}

	return filterAndReplaceSnapshot(ctx, repo, sn,
		filter, opts.DryRun, opts.Forget, metadata, "rewrite")
}

func filterAndReplaceSnapshot(ctx context.Context, repo restic.Repository, sn *restic.Snapshot,
	filter rewriteFilterFunc, dryRun bool, forget bool, newMetadata *snapshotMetadata, addTag string) (bool, error) {

	wg, wgCtx := errgroup.WithContext(ctx)
	repo.StartPackUploader(wgCtx, wg)

	var filteredTree restic.ID
	var summary *restic.SnapshotSummary
	wg.Go(func() error {
		var err error
		filteredTree, summary, err = filter(ctx, sn)
		if err != nil {
			return err
		}

		return repo.Flush(wgCtx)
	})
	err := wg.Wait()
	if err != nil {
		return false, err
	}

	if filteredTree.IsNull() {
		if dryRun {
			Verbosef("would delete empty snapshot\n")
		} else {
			if err = repo.RemoveUnpacked(ctx, restic.WriteableSnapshotFile, *sn.ID()); err != nil {
				return false, err
			}
			debug.Log("removed empty snapshot %v", sn.ID())
			Verbosef("removed empty snapshot %v\n", sn.ID().Str())
		}
		return true, nil
	}

	matchingSummary := true
	if summary != nil {
		matchingSummary = sn.Summary != nil && *summary == *sn.Summary
	}

	if filteredTree == *sn.Tree && newMetadata == nil && matchingSummary {
		debug.Log("Snapshot %v not modified", sn)
		return false, nil
	}

	debug.Log("Snapshot %v modified", sn)
	if dryRun {
		Verbosef("would save new snapshot\n")

		if forget {
			Verbosef("would remove old snapshot\n")
		}

		if newMetadata != nil && newMetadata.Time != nil {
			Verbosef("would set time to %s\n", newMetadata.Time)
		}

		if newMetadata != nil && newMetadata.Hostname != "" {
			Verbosef("would set hostname to %s\n", newMetadata.Hostname)
		}

		return true, nil
	}

	// Always set the original snapshot id as this essentially a new snapshot.
	sn.Original = sn.ID()
	sn.Tree = &filteredTree
	if summary != nil {
		sn.Summary = summary
	}

	if !forget {
		sn.AddTags([]string{addTag})
	}

	if newMetadata != nil && newMetadata.Time != nil {
		Verbosef("setting time to %s\n", *newMetadata.Time)
		sn.Time = *newMetadata.Time
	}

	if newMetadata != nil && newMetadata.Hostname != "" {
		Verbosef("setting host to %s\n", newMetadata.Hostname)
		sn.Hostname = newMetadata.Hostname
	}

	// Save the new snapshot.
	id, err := restic.SaveSnapshot(ctx, repo, sn)
	if err != nil {
		return false, err
	}
	Verbosef("saved new snapshot %v\n", id.Str())

	if forget {
		if err = repo.RemoveUnpacked(ctx, restic.WriteableSnapshotFile, *sn.ID()); err != nil {
			return false, err
		}
		debug.Log("removed old snapshot %v", sn.ID())
		Verbosef("removed old snapshot %v\n", sn.ID().Str())
	}
	return true, nil
}

func runRewrite(ctx context.Context, opts RewriteOptions, gopts GlobalOptions, args []string) error {
	if !opts.SnapshotSummary && opts.ExcludePatternOptions.Empty() && opts.Metadata.empty() {
		return errors.Fatal("Nothing to do: no excludes provided and no new metadata provided")
	}

	var (
		repo   *repository.Repository
		unlock func()
		err    error
	)

	if opts.Forget {
		Verbosef("create exclusive lock for repository\n")
		ctx, repo, unlock, err = openWithExclusiveLock(ctx, gopts, opts.DryRun)
	} else {
		ctx, repo, unlock, err = openWithAppendLock(ctx, gopts, opts.DryRun)
	}
	if err != nil {
		return err
	}
	defer unlock()

	snapshotLister, err := restic.MemorizeList(ctx, repo, restic.SnapshotFile)
	if err != nil {
		return err
	}

	bar := newIndexProgress(gopts.Quiet, gopts.JSON)
	if err = repo.LoadIndex(ctx, bar); err != nil {
		return err
	}

	changedCount := 0
	for sn := range FindFilteredSnapshots(ctx, snapshotLister, repo, &opts.SnapshotFilter, args) {
		Verbosef("\n%v\n", sn)
		changed, err := rewriteSnapshot(ctx, repo, sn, opts)
		if err != nil {
			return errors.Fatalf("unable to rewrite snapshot ID %q: %v", sn.ID().Str(), err)
		}
		if changed {
			changedCount++
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	Verbosef("\n")
	if changedCount == 0 {
		if !opts.DryRun {
			Verbosef("no snapshots were modified\n")
		} else {
			Verbosef("no snapshots would be modified\n")
		}
	} else {
		if !opts.DryRun {
			Verbosef("modified %v snapshots\n", changedCount)
		} else {
			Verbosef("would modify %v snapshots\n", changedCount)
		}
	}

	return nil
}
