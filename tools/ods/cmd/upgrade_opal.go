package cmd

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/onyx-dot-app/onyx/tools/ods/internal/git"
	"github.com/onyx-dot-app/onyx/tools/ods/internal/prompt"
)

const (
	opalUpgradeRepo      = "onyx-dot-app/onyx"
	opalUpgradeWorkflow  = "opal-upgrade.yml"
	opalUpgradePollLimit = 15 * time.Minute
)

// opalSemverRe matches a bare X.Y.Z version (no leading v).
var opalSemverRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// UpgradeOpalOptions holds options for the upgrade opal command.
type UpgradeOpalOptions struct {
	Bump     string
	Version  string
	Reviewer string
	DryRun   bool
	Yes      bool
	Wait     bool
}

// NewUpgradeOpalCommand creates the `ods upgrade opal` command.
func NewUpgradeOpalCommand() *cobra.Command {
	opts := &UpgradeOpalOptions{}

	cmd := &cobra.Command{
		Use:   "opal",
		Short: "Cut a new @onyx-ai/opal release via a bot PR",
		Long: `Cut a new @onyx-ai/opal release by dispatching the opal-upgrade workflow.

The command itself does no git work — it triggers the opal-upgrade.yml workflow,
which runs as the Onyx release bot and:
  1. Bumps the version in web/lib/opal/package.json and refreshes web/bun.lock
  2. Opens a PR from the bot, requesting you (or --reviewer) as the reviewer
  3. On merge, a follow-up workflow pushes the opal/vX.Y.Z tag, which builds and
     publishes the package to npm (release-opal.yml)

By default the patch version is bumped. Use --bump minor|major, or pin an exact
--version. The reviewer defaults to your GitHub login.

All GitHub operations run through the gh CLI, so authorization is enforced by
your gh credentials and GitHub's repo/workflow permissions.

Example usage:

    $ ods upgrade opal
    $ ods upgrade opal --bump minor
    $ ods upgrade opal --version 0.2.0`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			upgradeOpal(opts)
		},
	}

	cmd.Flags().StringVar(&opts.Bump, "bump", "patch", "Semver part to bump when --version is unset: patch|minor|major")
	cmd.Flags().StringVar(&opts.Version, "version", "", "Exact version to release (X.Y.Z, no leading v); overrides --bump")
	cmd.Flags().StringVar(&opts.Reviewer, "reviewer", "", "GitHub login to request as reviewer (default: your gh login)")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "Resolve inputs but skip dispatching the workflow")
	cmd.Flags().BoolVar(&opts.Yes, "yes", false, "Skip the confirmation prompt")
	cmd.Flags().BoolVar(&opts.Wait, "wait", false, "Wait for the dispatched workflow run to finish")

	return cmd
}

func upgradeOpal(opts *UpgradeOpalOptions) {
	git.CheckGitHubCLI()

	// Validate version/bump selection up front so we fail before dispatching.
	if opts.Version != "" {
		if !opalSemverRe.MatchString(opts.Version) {
			log.Fatalf("--version must be X.Y.Z with no leading v, got %q", opts.Version)
		}
	} else {
		switch opts.Bump {
		case "patch", "minor", "major":
		default:
			log.Fatalf("--bump must be one of patch|minor|major, got %q", opts.Bump)
		}
	}

	reviewer := opts.Reviewer
	if reviewer == "" {
		login, err := currentGitHubLogin()
		if err != nil {
			log.Fatalf("Failed to resolve your GitHub login (pass --reviewer): %v", err)
		}
		reviewer = login
	}

	target := "bump " + opts.Bump
	if opts.Version != "" {
		target = "version " + opts.Version
	}
	log.Infof("Opal release: %s, reviewer @%s", target, reviewer)

	if opts.DryRun {
		log.Warnf("[DRY RUN] Would dispatch %s in %s (%s, reviewer=%s)", opalUpgradeWorkflow, opalUpgradeRepo, target, reviewer)
		return
	}

	if !opts.Yes {
		if !prompt.Confirm(fmt.Sprintf("Cut a new opal release (%s) via a bot PR, reviewer @%s? (Y/n): ", target, reviewer)) {
			log.Info("Exiting...")
			return
		}
	}

	priorRunID, err := latestWorkflowRunID(opalUpgradeRepo, opalUpgradeWorkflow, "workflow_dispatch", "")
	if err != nil {
		log.Fatalf("Failed to query existing opal-upgrade runs: %v", err)
	}
	log.Debugf("Most recent prior opal-upgrade run id: %d", priorRunID)

	inputs := map[string]string{"bump": opts.Bump, "reviewer": reviewer}
	if opts.Version != "" {
		inputs["version"] = opts.Version
	}

	log.Infof("Dispatching %s in %s...", opalUpgradeWorkflow, opalUpgradeRepo)
	if err := dispatchWorkflow(opalUpgradeRepo, opalUpgradeWorkflow, inputs); err != nil {
		log.Fatalf("Failed to dispatch opal-upgrade workflow: %v", err)
	}

	log.Info("Waiting for the workflow run to start...")
	run, err := waitForNewRun(opalUpgradeRepo, opalUpgradeWorkflow, "workflow_dispatch", "", priorRunID)
	if err != nil {
		log.Fatalf("Failed to find dispatched opal-upgrade run: %v", err)
	}
	log.Infof("opal-upgrade run started: %s", run.URL)

	if !opts.Wait {
		log.Info("The workflow will open a bot PR and request your review. Watch the run above.")
		return
	}

	if err := waitForRunCompletion(opalUpgradeRepo, run.DatabaseID, opalUpgradePollLimit, "opal-upgrade"); err != nil {
		log.Fatalf("opal-upgrade did not complete successfully: %v", err)
	}
	log.Info("opal-upgrade completed. Review the bot PR it opened.")
}

// currentGitHubLogin returns the login of the gh-authenticated user, used as
// the default PR reviewer.
func currentGitHubLogin() (string, error) {
	output, err := exec.Command("gh", "api", "user", "--jq", ".login").Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("%w: %s", err, string(exitErr.Stderr))
		}
		return "", err
	}
	login := strings.TrimSpace(string(output))
	if login == "" {
		return "", fmt.Errorf("gh returned an empty login")
	}
	return login, nil
}
