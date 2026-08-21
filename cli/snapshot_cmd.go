package main

import (
	"fmt"
	"strconv"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	fuse "github.com/folsomintel/fuse/sdks/go"
)

func newSnapshotCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "snapshot",
		Aliases: []string{"snapshots", "snap", "snaps"},
		Short:   "Manage VM snapshots",
	}
	cmd.AddCommand(
		newSnapCreateCmd(),
		newSnapListCmd(),
		newSnapGetCmd(),
		newSnapDeleteCmd(),
		newSnapRestoreCmd(),
	)
	return cmd
}

func newSnapCreateCmd() *cobra.Command {
	var (
		comment   string
		mode      string
		retention int64
		exportRef string
		metadata  []string
		live      bool
	)
	cmd := &cobra.Command{
		Use:   "create <vm-id>",
		Short: "Create a snapshot of a running environment",
		Long: "create snapshots the given environment (vm). the vm must be RUNNING and the\n" +
			"provider must support snapshots. with a tty and no flags, comment/mode/retention\n" +
			"are collected interactively.\n\n" +
			"by default the snapshot is disk-only, so restoring it cold-boots the guest.\n" +
			"--live also captures guest memory and vcpu state, so restoring resumes the\n" +
			"guest where it stopped; it costs the vm's full configured ram on every\n" +
			"snapshot, pauses the guest while it runs, and pins the artifact to the host\n" +
			"that took it. the 'kind' on the result is what was actually written, so check\n" +
			"it rather than assuming --live was honoured.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			vmID := args[0]
			changed := cmd.Flags().Changed("comment") || cmd.Flags().Changed("mode") ||
				cmd.Flags().Changed("retention") || cmd.Flags().Changed("export-ref")
			if !changed && isInteractive() {
				if mode == "" {
					mode = "manual"
				}
				retS := strconv.FormatInt(retention, 10)
				err := runForm(huh.NewGroup(
					huh.NewInput().Title("Comment").Value(&comment),
					huh.NewSelect[string]().Title("Mode").Options(huh.NewOptions("manual", "auto")...).Value(&mode),
					huh.NewInput().Title("Retention (seconds, 0 = keep forever)").Value(&retS).Validate(validateInt),
				))
				if err != nil {
					return err
				}
				retention, _ = strconv.ParseInt(retS, 10, 64)
			}
			meta, err := parseKeyVals(metadata)
			if err != nil {
				return err
			}
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			s, err := cl.Snapshots.Create(cmd.Context(), vmID, fuse.SnapshotRequest{
				Comment:          comment,
				Mode:             mode,
				RetentionSeconds: retention,
				Metadata:         meta,
				ExportRef:        exportRef,
				Live:             live,
			})
			if err != nil {
				return friendly(err)
			}
			successf("creating snapshot %s of %s", s.ID, vmID)
			if app.isJSON() {
				return printJSON(s)
			}
			renderSnapDetail(s)
			return nil
		},
	}
	cmd.Flags().StringVar(&comment, "comment", "", "snapshot comment")
	cmd.Flags().StringVar(&mode, "mode", "", "mode: manual | auto")
	cmd.Flags().Int64Var(&retention, "retention", 0, "retention in seconds (0 = keep forever)")
	cmd.Flags().StringVar(&exportRef, "export-ref", "", "export reference")
	cmd.Flags().StringArrayVar(&metadata, "metadata", nil, "metadata as key=value (repeatable)")
	// deliberately not part of the `changed` check above: like --metadata, it
	// is not one of the fields the interactive form collects, so passing it
	// alone on a tty should still prompt for comment/mode/retention.
	cmd.Flags().BoolVar(&live, "live", false, "also capture guest memory and vcpu state (resumable restore; costs the vm's full ram per snapshot and pauses the guest)")
	return cmd
}

func newSnapListCmd() *cobra.Command {
	var (
		vmID     string
		taskID   string
		tenantID string
		state    string
		limit    int
		cursor   string
		allPages bool
	)
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List snapshots (filter by vm/task/tenant/state)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, cur, err := app.client()
			if err != nil {
				return err
			}
			if cur.ActiveHost != "" && vmID == "" && taskID == "" {
				warnf("note: snapshot listing is not host-scoped (no host filter in the api); showing all matching snapshots")
			}

			var (
				snaps []fuse.Snapshot
				next  string
			)
			if allPages {
				snaps, err = cl.Snapshots.List(cmd.Context(), fuse.ListSnapshotsOptions{
					VMID:     vmID,
					TaskID:   taskID,
					TenantID: tenantID,
					State:    state,
					Cursor:   cursor,
				})
			} else {
				var page *fuse.SnapshotPage
				page, err = cl.Snapshots.ListPage(cmd.Context(), fuse.ListSnapshotsOptions{
					VMID:     vmID,
					TaskID:   taskID,
					TenantID: tenantID,
					State:    state,
					Limit:    limit,
					Cursor:   cursor,
				})
				if page != nil {
					snaps, next = page.Snapshots, page.NextCursor
				}
			}
			if err != nil {
				return friendly(err)
			}
			if app.isJSON() {
				return printJSON(fuse.SnapshotPage{Snapshots: snaps, NextCursor: next})
			}
			rows := make([][]string, 0, len(snaps))
			for _, s := range snaps {
				rows = append(rows, []string{
					s.ID, s.VMID, dash(s.State), dash(s.Mode), humanBytes(s.SizeBytes),
					dash(s.ParentSnapshotID), shortTime(s.CreatedAt), dash(s.Comment),
				})
			}
			renderTable([]string{"ID", "VM ID", "STATE", "MODE", "SIZE", "PARENT", "CREATED", "COMMENT"}, rows)
			if next != "" {
				warnf("more results: rerun with --cursor %s (or pass --all-pages to fetch everything)", next)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&vmID, "vm-id", "", "filter by vm id")
	cmd.Flags().StringVar(&taskID, "task-id", "", "filter by task id")
	cmd.Flags().StringVar(&tenantID, "tenant-id", "", "filter by tenant id")
	cmd.Flags().StringVar(&state, "state", "", "filter by state (creating|ready|restoring|deleting|error)")
	cmd.Flags().IntVar(&limit, "limit", 0, "max results per page (default 50, max 200)")
	cmd.Flags().StringVar(&cursor, "cursor", "", "resume from a cursor returned by a previous page")
	cmd.Flags().BoolVar(&allPages, "all-pages", false, "auto-paginate and fetch every matching snapshot")
	return cmd
}

func newSnapGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <id>",
		Short: "Show a snapshot's details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			s, err := cl.Snapshots.Get(cmd.Context(), args[0])
			if err != nil {
				return friendly(err)
			}
			if app.isJSON() {
				return printJSON(s)
			}
			renderSnapDetail(s)
			return nil
		},
	}
}

// snapKind resolves an empty kind to "disk". Empty is not a third kind, it is
// a server that predates the field, and every such snapshot is disk-only.
func snapKind(kind string) string {
	if kind == "" {
		return "disk"
	}
	return kind
}

func renderSnapDetail(s *fuse.Snapshot) {
	retention := "keep forever"
	if s.RetentionUntil != nil {
		retention = shortTime(*s.RetentionUntil)
	}
	pairs := [][2]string{
		{"id", s.ID},
		{"vm id", s.VMID},
		{"task id", dash(s.TaskID)},
		{"tenant id", dash(s.TenantID)},
		{"parent", dash(s.ParentSnapshotID)},
		{"mode", dash(s.Mode)},
		// what was actually written, not what --live asked for. an older
		// server omits it entirely, which reads as disk.
		{"kind", snapKind(s.Kind)},
		{"state", stateStyle(s.State)},
		{"size", humanBytes(s.SizeBytes)},
		{"retention", retention},
		{"created", shortTime(s.CreatedAt)},
		{"updated", shortTime(s.UpdatedAt)},
		{"comment", dash(s.Comment)},
		{"export ref", dash(s.ExportRef)},
	}
	if s.LastError != "" {
		pairs = append(pairs, [2]string{"error", styleBad.Render(s.LastError)})
	}
	renderDetail(pairs)
	for i, ex := range s.Exports {
		infof("export[%d]: %s -> %s (%s)", i, ex.Destination, ex.Status, dash(ex.LastError))
	}
}

func newSnapDeleteCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "delete <id>",
		Aliases: []string{"rm"},
		Short:   "Delete a snapshot (leaf-only; deletes the provider artifact)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if !yes {
				ok, err := confirm(fmt.Sprintf("Delete snapshot %q (also deletes the stored artifact)?", id))
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("aborted (use --yes to skip confirmation)")
				}
			}
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			if err := cl.Snapshots.Delete(cmd.Context(), id); err != nil {
				return friendly(err)
			}
			successf("deleted snapshot %q", id)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}

func newSnapRestoreCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "restore <id>",
		Short: "Restore a snapshot in place (onto its original, running VM)",
		Long: "restore is in-place only: it restores the snapshot onto its original vm,\n" +
			"which must be running. there is no cross-host restore, clone, or fork.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if !yes {
				ok, err := confirm(fmt.Sprintf("Restore snapshot %q in place (overwrites the running VM)?", id))
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("aborted (use --yes to skip confirmation)")
				}
			}
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			if err := cl.Snapshots.Restore(cmd.Context(), id); err != nil {
				return friendly(err)
			}
			successf("restoring snapshot %q", id)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}
