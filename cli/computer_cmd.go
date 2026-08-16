package main

// fuse computer: drive a desktop environment's screen, mouse and keyboard by
// hand. This is the human door to the same orchestrator endpoint an agent
// loop uses; it exists so a desktop session can be inspected without writing
// one.

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	fuse "github.com/folsomintel/fuse/sdks/go"
	"github.com/spf13/cobra"
)

func newComputerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "computer",
		Short: "Drive a desktop environment's screen, mouse and keyboard",
		Long: "Drive a desktop environment by hand: capture the screen, click, type,\n" +
			"and send key combos. Requires an environment booted from a desktop image\n" +
			"(see the desktop environments guide); on any other image the server\n" +
			"answers 503 with a reason.\n\n" +
			"These commands are for poking at a session interactively. An agent loop\n" +
			"drives the same endpoint through the SDKs instead.",
	}
	cmd.AddCommand(
		newComputerScreenshotCmd(),
		newComputerClickCmd(),
		newComputerTypeCmd(),
		newComputerKeyCmd(),
	)
	return cmd
}

// computerAction runs one action against an environment and hands back the
// response, with client construction and error friendliness shared across the
// subcommands.
func computerAction(cmd *cobra.Command, vmID string, action fuse.ComputerActionRequest) (*fuse.ComputerActionResponse, error) {
	cl, _, err := app.client()
	if err != nil {
		return nil, err
	}
	res, err := cl.Environments.Computer(cmd.Context(), vmID, action)
	if err != nil {
		return nil, friendly(err)
	}
	return res, nil
}

func newComputerScreenshotCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "screenshot <id>",
		Short: "Capture the environment's screen to a PNG",
		Long: "Capture the environment's screen and write it as a PNG.\n\n" +
			"  fuse computer screenshot fuse-task-1\n" +
			"  fuse computer screenshot fuse-task-1 --out shot.png\n" +
			"  fuse computer screenshot fuse-task-1 --out - > shot.png\n\n" +
			"--out - writes the raw PNG to stdout for piping. The flag is --out\n" +
			"rather than -o because -o is the global output-format flag.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := computerAction(cmd, args[0], fuse.ComputerActionRequest{Action: "screenshot"})
			if err != nil {
				return err
			}
			png, err := base64.StdEncoding.DecodeString(res.Screenshot)
			if err != nil {
				return fmt.Errorf("server returned an unreadable screenshot: %w", err)
			}
			if len(png) == 0 {
				return errors.New("server returned an empty screenshot")
			}
			if out == "-" {
				_, err := os.Stdout.Write(png)
				return err
			}
			if err := os.WriteFile(out, png, 0o644); err != nil {
				return err
			}
			if app.isJSON() {
				return printJSON(map[string]any{"path": out, "bytes": len(png)})
			}
			successf("wrote %s (%s)", out, humanBytes(int64(len(png))))
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "screenshot.png", "file to write the PNG to; - for stdout")
	return cmd
}

func newComputerClickCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "click <id> <x> <y>",
		Short: "Left-click at a coordinate on the environment's screen",
		Long: "Left-click at a coordinate, measured in pixels from the display's\n" +
			"top-left corner.\n\n" +
			"  fuse computer click fuse-task-1 512 384",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			x, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("x must be an integer, got %q", args[1])
			}
			y, err := strconv.Atoi(args[2])
			if err != nil {
				return fmt.Errorf("y must be an integer, got %q", args[2])
			}
			res, err := computerAction(cmd, args[0], fuse.ComputerActionRequest{
				Action:     "left_click",
				Coordinate: []int{x, y},
			})
			if err != nil {
				return err
			}
			if app.isJSON() {
				return printJSON(res)
			}
			successf("clicked (%d, %d)", x, y)
			return nil
		},
	}
}

func newComputerTypeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "type <id> <text>...",
		Short: "Type text into the environment",
		Long: "Type text into whatever currently has focus. Multiple arguments are\n" +
			"joined with single spaces, so quoting is only needed to preserve other\n" +
			"whitespace.\n\n" +
			"  fuse computer type fuse-task-1 hello world",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			text := strings.Join(args[1:], " ")
			res, err := computerAction(cmd, args[0], fuse.ComputerActionRequest{
				Action: "type",
				Text:   text,
			})
			if err != nil {
				return err
			}
			if app.isJSON() {
				return printJSON(res)
			}
			successf("typed %d characters", len(text))
			return nil
		},
	}
}

func newComputerKeyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "key <id> <combo>",
		Short: "Send a key combo to the environment",
		Long: "Send a key or combo, in xdotool keysym notation: letters, digits,\n" +
			"underscore and + only.\n\n" +
			"  fuse computer key fuse-task-1 Return\n" +
			"  fuse computer key fuse-task-1 ctrl+shift+t",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := computerAction(cmd, args[0], fuse.ComputerActionRequest{
				Action: "key",
				Text:   args[1],
			})
			if err != nil {
				return err
			}
			if app.isJSON() {
				return printJSON(res)
			}
			successf("sent %s", args[1])
			return nil
		},
	}
}
