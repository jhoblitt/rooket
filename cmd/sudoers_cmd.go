package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/run"
)

var sudoersUser string

var sudoersCmd = &cobra.Command{
	Use:   "sudoers",
	Short: "Manage the sudoers rule that lets rooket configure iSCSI without prompting",
	Long: `sudoers manages ` + sudoersPath + `, which grants passwordless root for the
targetcli and iscsiadm operations rooket performs in ` + "`block setup`" + `,
` + "`block teardown`" + `, and ` + "`down --all`" + `.

Without this rule rooket still works: it falls back to a single pkexec prompt
per privileged run.

SECURITY: the grant is root-equivalent. targetcli can expose any file as a
fileio backstore and rooket's disk images are user-writable, so anyone holding
it can obtain root. It suits a single-user development workstation and is not a
privilege boundary.
`,
	// PersistentPreRunE on the root command probes for a usable container
	// engine, which sudoers has no use for and must not fail on: the rule can
	// be installed on a host that has neither podman nor docker. Cobra runs the
	// nearest PersistentPreRunE, so this covers all four subcommands. It still
	// applies --timestamps/--color, matching everything else root.go's version
	// does short of the engine probe.
	PersistentPreRunE: func(*cobra.Command, []string) error {
		run.SetTimestamps(timestampsFlag)
		useColor, err := resolveColor(colorFlag, os.Stdout)
		if err != nil {
			return err
		}
		run.SetColor(useColor)
		return nil
	},
}

var sudoersPrintCmd = &cobra.Command{
	Use:           "print",
	Short:         "Print the sudoers rule rooket would install",
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		user, paths, err := grantTarget(sudoersUser)
		if err != nil {
			return err
		}
		rendered, err := renderSudoers(user, paths)
		if err != nil {
			return err
		}
		fmt.Fprint(cmd.OutOrStdout(), rendered)
		return nil
	},
}

var sudoersStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Report whether the passwordless rule is installed and current",
	Long: `status exits 0 when the installed rule matches what this rooket would
generate, and 1 otherwise.
`,
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		msg, ok, err := sudoersState(sudoersUser)
		if err != nil {
			return err
		}
		run.Fprintf(cmd.OutOrStdout(), "%s\n", msg)
		if !ok {
			return fmt.Errorf("run `rooket sudoers install`")
		}
		return nil
	},
}

var sudoersInstallCmd = &cobra.Command{
	Use:           "install",
	Short:         "Install or update the sudoers rule (requires one authentication)",
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(_ *cobra.Command, _ []string) error {
		return sudoersInstall(sudoersUser)
	},
}

var sudoersUninstallCmd = &cobra.Command{
	Use:           "uninstall",
	Short:         "Remove the sudoers rule (requires one authentication)",
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(_ *cobra.Command, _ []string) error {
		return sudoersUninstall()
	},
}

// grantTarget resolves the user to grant and the absolute, trusted paths of
// every command in the vocabulary. An empty user resolves through
// defaultGrantUser; a non-empty one is validated and used as-is, so the
// caller's choice of user is always what gets granted.
func grantTarget(user string) (string, map[string]string, error) {
	if user == "" {
		var err error
		if user, err = defaultGrantUser(); err != nil {
			return "", nil, err
		}
	} else if err := validGrantUser(user); err != nil {
		return "", nil, err
	}
	paths, err := resolveCommandPaths()
	if err != nil {
		return "", nil, err
	}
	return user, paths, nil
}

// sudoersState compares the installed rule against a fresh render. It cannot
// distinguish "absent" from "installed by a rooket that predates a vocabulary
// change", because both make the pinned cat unavailable — and both are fixed
// by reinstalling.
func sudoersState(userFlag string) (string, bool, error) {
	user, paths, err := grantTarget(userFlag)
	if err != nil {
		return "", false, err
	}
	installed, ok := readInstalledSudoersFunc()
	if !ok {
		return "not installed", false, nil
	}
	rendered, err := renderSudoers(user, paths)
	if err != nil {
		return "", false, err
	}
	if installed != rendered {
		return "stale", false, nil
	}
	return "up to date", true, nil
}

func sudoersInstall(userFlag string) error {
	if userFlag != "" {
		if err := validGrantUser(userFlag); err != nil {
			return err
		}
	}
	if os.Geteuid() != 0 {
		return reexecAsRoot("install", userFlag)
	}
	user, paths, err := grantTarget(userFlag)
	if err != nil {
		return err
	}
	rendered, err := renderSudoers(user, paths)
	if err != nil {
		return err
	}

	// sudo's #includedir skips any file name containing a dot, so the staging
	// file is inert even while it is being written and checked. CreateTemp
	// gives each concurrent install its own staging file rather than a fixed
	// name two simultaneous installs could race on; the "rooket.tmp" prefix
	// keeps the required dot regardless of the random suffix CreateTemp adds.
	f, err := os.CreateTemp(filepath.Dir(sudoersPath), "rooket.tmp")
	if err != nil {
		return fmt.Errorf("create staging file: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)

	if err := f.Chmod(0o440); err != nil {
		f.Close()
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if _, err := f.Write([]byte(rendered)); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}

	if out, err := exec.Command("visudo", "-cf", tmp).CombinedOutput(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("visudo not found in $PATH: %w", err)
		}
		return fmt.Errorf("visudo rejected the generated rule: %w\n%s", err, out)
	}
	if err := os.Chown(tmp, 0, 0); err != nil {
		return fmt.Errorf("chown %s: %w", tmp, err)
	}
	if err := os.Chmod(tmp, 0o440); err != nil {
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, sudoersPath); err != nil {
		return fmt.Errorf("install %s: %w", sudoersPath, err)
	}
	run.Printf("installed %s granting %s\n", sudoersPath, user)
	return nil
}

func sudoersUninstall() error {
	if os.Geteuid() != 0 {
		return reexecAsRoot("uninstall", "")
	}
	if err := os.Remove(sudoersPath); err != nil {
		if os.IsNotExist(err) {
			run.Printf("%s is not installed\n", sudoersPath)
			return nil
		}
		return err
	}
	run.Printf("removed %s\n", sudoersPath)
	return nil
}

// reexecAsRoot re-runs this rooket under sudo. Resolving the command paths
// then happens under sudo's secure_path rather than the caller's $PATH.
func reexecAsRoot(verb, userFlag string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	argv := []string{"--", exe, "sudoers", verb}
	if userFlag != "" {
		argv = append(argv, "--user", userFlag)
	}
	run.Printf("==> re-running under sudo (you may be prompted to authenticate)\n")
	return run.Cmd("sudo", argv...)
}

func init() {
	rootCmd.AddCommand(sudoersCmd)
	sudoersCmd.AddCommand(sudoersPrintCmd, sudoersStatusCmd, sudoersInstallCmd, sudoersUninstallCmd)

	for _, c := range []*cobra.Command{sudoersPrintCmd, sudoersStatusCmd, sudoersInstallCmd} {
		c.Flags().StringVar(&sudoersUser, "user", "", "user to grant (default: $SUDO_USER, else the current user)")
	}
}
