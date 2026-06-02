package cmd

import (
	"bufio"
	"crypto/rand"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/monsterdept/krapow/internal/auth"
	"github.com/monsterdept/krapow/internal/githubapi"
	"github.com/monsterdept/krapow/internal/hostmac"
	"github.com/monsterdept/krapow/internal/imagebuild"
	"github.com/monsterdept/krapow/internal/incus"
	"github.com/monsterdept/krapow/internal/macssh"
	"github.com/monsterdept/krapow/internal/provision"
	"github.com/monsterdept/krapow/internal/sshkeys"
	"github.com/monsterdept/krapow/internal/state"
	"github.com/monsterdept/krapow/internal/tart"
	"github.com/monsterdept/krapow/internal/tui"
	"github.com/monsterdept/krapow/internal/winssh"
	"github.com/spf13/cobra"
)

// randomSuffix returns a 6-char [a-z0-9] string for runner names.
func randomSuffix() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano()&0xffffff)
	}
	out := make([]byte, 6)
	for i, x := range b {
		out[i] = alphabet[int(x)%len(alphabet)]
	}
	return string(out)
}

func initCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "init",
		Short: "Create a runner VM and register it with GitHub",
	}
	c.AddCommand(initLinuxCmd(), initWinCmd(), initMacCmd())
	return c
}

func initLinuxCmd() *cobra.Command {
	var name, labels, repo, org, isolation string
	var plain bool
	// On macOS hosts, `init linux` produces a Linux ARM VM via tart instead of
	// going through Incus (which doesn't exist on macOS). The labels default
	// gets `,arm64` appended in that case so workflows can target it sensibly.
	defaultLabels := "self-hosted,linux,krapow"
	if runtime.GOOS == "darwin" {
		defaultLabels = "self-hosted,linux,arm64,krapow"
	}
	c := &cobra.Command{
		Use:     "linux",
		Aliases: []string{"lin"},
		Short:   "Launch a Linux runner (Ubuntu via Incus on Linux hosts; Ubuntu ARM via Tart on macOS hosts)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(linuxKind, name, labels, repo, org, isolation, plain, 0)
		},
	}
	addScopeFlags(c, &repo, &org)
	c.Flags().StringVar(&name, "name", "", "instance + runner name (default: linux-runner-<6 alphanum>)")
	c.Flags().StringVar(&labels, "labels", defaultLabels, "comma-separated runner labels")
	c.Flags().StringVar(&isolation, "isolation", "", "isolation mechanism (default per host+kind; see README)")
	c.Flags().BoolVar(&plain, "plain", false, "disable the interactive TUI and print plain status lines")
	return c
}

func initMacCmd() *cobra.Command {
	var name, labels, repo, org, isolation string
	var plain bool
	c := &cobra.Command{
		Use:     "mac",
		Aliases: []string{"macos"},
		Short:   "Launch a macOS runner on this host (default: --isolation host, no VM)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "darwin" {
				return fmt.Errorf("`krapow init mac` requires a macOS host (Tart wraps Apple's Virtualization.framework); current GOOS=%s", runtime.GOOS)
			}
			return runInit(macKind, name, labels, repo, org, isolation, plain, 0)
		},
	}
	addScopeFlags(c, &repo, &org)
	c.Flags().StringVar(&name, "name", "", "instance + runner name (default: mac-runner-<6 alphanum>)")
	c.Flags().StringVar(&labels, "labels", "self-hosted,macOS,arm64,krapow", "comma-separated runner labels")
	c.Flags().StringVar(&isolation, "isolation", "", "isolation mechanism (default per host+kind; see README)")
	c.Flags().BoolVar(&plain, "plain", false, "disable the interactive TUI and print plain status lines")
	return c
}

func initWinCmd() *cobra.Command {
	var name, labels, repo, org, isolation string
	var yesBuild, plain bool
	var diskGiB int
	c := &cobra.Command{
		Use:     "win",
		Aliases: []string{"windows"},
		Short:   "Launch a Windows Incus VM as a runner (auto-bakes base image on first run)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInitWin(name, labels, repo, org, isolation, yesBuild, plain, diskGiB)
		},
	}
	addScopeFlags(c, &repo, &org)
	c.Flags().StringVar(&name, "name", "", "instance + runner name (default: win-runner-<6 alphanum>)")
	c.Flags().StringVar(&labels, "labels", "self-hosted,windows,krapow", "comma-separated runner labels")
	c.Flags().StringVar(&isolation, "isolation", "", "isolation mechanism (windows is vm-only)")
	c.Flags().BoolVarP(&yesBuild, "yes", "y", false, "skip the confirmation prompt before kicking off a base-image build")
	c.Flags().BoolVar(&plain, "plain", false, "disable the interactive TUI and print plain status lines")
	c.Flags().IntVar(&diskGiB, "disk", winDefaultDiskGiB, fmt.Sprintf("root disk size in GiB (min %d — matches the bake image floor; C: is grown to fill the disk on first boot)", winMinDiskGiB))
	return c
}

// addScopeFlags wires --repo and --org as mutually exclusive flags; exactly
// one must be provided. The validation happens in resolveScope() so the
// command can surface a clear error rather than cobra's terser "either ... or"
// message — but MarkFlagsMutuallyExclusive still blocks the both-set case.
func addScopeFlags(c *cobra.Command, repo, org *string) {
	c.Flags().StringVar(repo, "repo", "", "GitHub repository in owner/name form (repo-scoped runner)")
	c.Flags().StringVar(org, "org", "", "GitHub organization (org-scoped runner; needs an org-admin token)")
	c.MarkFlagsMutuallyExclusive("repo", "org")
	c.MarkFlagsOneRequired("repo", "org")
}

// parseRepo accepts either "owner/name" or a full https URL and returns the
// normalized "owner/name" form. Used by `init --repo`.
func parseRepo(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ".git")
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return "", fmt.Errorf("--repo %q: %w", s, err)
		}
		s = strings.Trim(u.Path, "/")
	}
	if strings.Count(s, "/") != 1 || strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") {
		return "", fmt.Errorf("--repo %q is not owner/name", s)
	}
	return s, nil
}

// scopeHint returns a "\n  hint: ..." suffix for the preflight error when the
// failure looks like a missing-scope 403 on an org endpoint. Empty otherwise.
// `gh auth login` defaults don't include admin:org, so this is the single most
// common cause of an org-runner init failing — naming the exact fix saves the
// user a docs trip.
func scopeHint(scope string, err error) string {
	if scope != "org" || err == nil || !strings.Contains(err.Error(), "403") {
		return ""
	}
	return "\n\n  hint: org runners need the admin:org scope; try `gh auth refresh -h github.com -s admin:org` (or use a PAT with the org's 'Self-hosted runners: read & write' permission)"
}

// resolveScope normalizes the --repo/--org flag pair into (ownerOrRepo, scope,
// apiTarget). Cobra's MarkFlagsOneRequired/MutuallyExclusive guarantee exactly
// one is set, so the empty-string check is just a belt-and-braces fallback.
func resolveScope(repoFlag, orgFlag string) (owner, scope, target string, err error) {
	switch {
	case repoFlag != "":
		owner, err = parseRepo(repoFlag)
		if err != nil {
			return "", "", "", err
		}
		return owner, "repo", "repos/" + owner, nil
	case orgFlag != "":
		owner, err = parseOrg(orgFlag)
		if err != nil {
			return "", "", "", err
		}
		return owner, "org", "orgs/" + owner, nil
	default:
		return "", "", "", fmt.Errorf("either --repo or --org is required")
	}
}

// parseOrg accepts either a bare org slug or a full https URL like
// https://github.com/orgname and returns the normalized slug. Used by
// `init --org`.
func parseOrg(s string) (string, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return "", fmt.Errorf("--org %q: %w", s, err)
		}
		s = strings.Trim(u.Path, "/")
	}
	if s == "" || strings.Contains(s, "/") {
		return "", fmt.Errorf("--org %q is not a bare organization slug", s)
	}
	return s, nil
}

type kind int

const (
	linuxKind kind = iota
	windowsKind
	macKind
)

// resolveIsolation maps the user's --isolation flag (or "" for default) to a
// concrete value validated against what krapow actually implements for this
// (kind, host) today.
//
// The first entry in each list is the default when --isolation is empty.
// On Linux hosts, linux runners default to "container" (Incus system
// container — sub-second boot); macOS hosts have no Incus, so linux runners
// there go through the Tart "vm" path.
func resolveIsolation(k kind, userInput string) (string, error) {
	linuxAllowed := []string{"vm"}
	if runtime.GOOS == "linux" {
		linuxAllowed = []string{"container", "vm"}
	}
	allowed := map[kind][]string{
		linuxKind:   linuxAllowed,
		macKind:     {"host", "vm"}, // "host" = run as current user under sandboxed HOME (default)
		windowsKind: {"vm"},         // VM-only forever (no host/container path for Windows)
	}
	values := allowed[k]
	if userInput == "" {
		return values[0], nil
	}
	for _, v := range values {
		if v == userInput {
			return v, nil
		}
	}
	return "", fmt.Errorf("--isolation %q is not valid for %s runners on this host; allowed: %s",
		userInput, kindName(k), strings.Join(values, ", "))
}

var (
	linuxImage    = envOr("KRAPOW_LINUX_IMAGE", "images:ubuntu/noble/cloud")
	windowsImage  = envOr("KRAPOW_WIN_IMAGE", "local:win-runner-base")
	macImage      = envOr("KRAPOW_MAC_IMAGE", "ghcr.io/cirruslabs/macos-sequoia-xcode:latest")
	linuxARMImage = envOr("KRAPOW_LINUX_ARM_IMAGE", "ghcr.io/cirruslabs/ubuntu-runner-arm64:24.04")
)

// winMinDiskGiB is the floor for --disk on `init win`. The bake VM publishes a
// 60 GiB image, and Incus refuses to clone into a smaller volume ("Source image
// size exceeds specified volume size"). winDefaultDiskGiB matches that floor to
// preserve historical behavior when --disk is omitted.
const (
	winMinDiskGiB     = 60
	winDefaultDiskGiB = 60
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func runInitWin(name, labels, repo, org, isolation string, yesBuild, plain bool, diskGiB int) error {
	if diskGiB < winMinDiskGiB {
		return fmt.Errorf("--disk %d is below the minimum of %d GiB (the bake image is %d GiB and Incus refuses to clone into a smaller volume)", diskGiB, winMinDiskGiB, winMinDiskGiB)
	}
	exists, err := incus.ImageExists(windowsImage)
	if err != nil {
		return err
	}
	if !exists {
		if !yesBuild {
			fmt.Printf("Base image %q not found.\n", windowsImage)
			fmt.Printf("krapow will bake it now: download Windows Server 2022 Eval + virtio drivers, run\n")
			fmt.Printf("an unattended install, sysprep, and publish — about 45–90 minutes total.\n")
			fmt.Printf("Proceed? [y/N] ")
			if !readYes() {
				return fmt.Errorf("aborted; rerun with -y to skip this prompt")
			}
		}
		if err := imagebuild.Build("win-runner-base"); err != nil {
			return err
		}
	}
	return runInit(windowsKind, name, labels, repo, org, isolation, plain, diskGiB)
}

func readYes() bool {
	s := bufio.NewScanner(os.Stdin)
	if !s.Scan() {
		return false
	}
	a := strings.ToLower(strings.TrimSpace(s.Text()))
	return a == "y" || a == "yes"
}

// initContext bundles everything a phase needs.
type initContext struct {
	gh        *githubapi.Client
	kind      kind
	name      string
	labels    string
	owner     string // "owner/name" for repo runners, or "orgname" for org runners
	scope     string // "repo" or "org"
	target    string // API path prefix: "repos/owner/name" or "orgs/orgname"
	ownerURL  string // "https://github.com/owner/name" or "https://github.com/orgname"
	isolation string // resolved isolation: "vm" today; "host"/"container" land in later phases
	regToken  string
	vmIP      string // populated by Windows ssh phase
	diskGiB   int    // root disk size in GiB; only consulted on the windows path (0 elsewhere)
}

func runInit(k kind, name, labels, repoFlag, orgFlag, isolationFlag string, plain bool, diskGiB int) error {
	owner, scope, target, err := resolveScope(repoFlag, orgFlag)
	if err != nil {
		return err
	}
	isolation, err := resolveIsolation(k, isolationFlag)
	if err != nil {
		return err
	}
	tok, _, err := auth.Token()
	if err != nil {
		return err
	}
	gh := githubapi.New(tok)
	// Preflight: confirm the token can see this target's runners before we boot
	// a VM. Cheaper to fail here than 5 minutes into a tart pull.
	if _, err := gh.ListRunners(target); err != nil {
		return fmt.Errorf("cannot access %s with current token: %w%s", owner, err, scopeHint(scope, err))
	}
	if name == "" {
		name = fmt.Sprintf("%s-%s", kindPrefix(k), randomSuffix())
	}
	if s, _ := state.Load(name); s != nil {
		return fmt.Errorf("runner %q already exists in krapow state", name)
	}

	ic := &initContext{
		gh:        gh,
		kind:      k,
		name:      name,
		labels:    labels,
		owner:     owner,
		scope:     scope,
		target:    target,
		ownerURL:  "https://github.com/" + owner,
		isolation: isolation,
		diskGiB:   diskGiB,
	}

	phases := phasesFor(k, ic.isolation)
	runner := tui.New(name, phases, plain)
	incus.StreamOut = runner.Logger()
	incus.StreamErr = runner.Logger()
	winssh.StreamOut = runner.Logger()
	winssh.StreamErr = runner.Logger()
	tart.StreamOut = runner.Logger()
	tart.StreamErr = runner.Logger()
	macssh.StreamOut = runner.Logger()

	var workErr error
	go func() {
		defer func() { runner.Finish(workErr) }()
		workErr = doInit(runner, ic)
	}()

	if err := runner.Run(); err != nil {
		return err
	}
	if workErr != nil {
		cleanupFailedInit(ic)
		return workErr
	}
	fmt.Printf("==> %s registered (%s)\n", ic.name, kindName(k))
	return nil
}

// cleanupFailedInit best-effort destroys anything a failed init managed to
// create: the GitHub runner registration (only present past the Activate
// phase, but FindRunner is cheap), the VM (tart or incus), and the local
// state file. Each step ignores errors and logs to stderr — we're already
// on an error path and shouldn't mask the original failure.
//
// The branch on state.Load handles the case where init died before saving
// state: in that case the VM doesn't exist either, so there's nothing local
// to clean. We still try the GitHub side because the registration token
// could in theory have been used by a half-running guest.
func cleanupFailedInit(ic *initContext) {
	fmt.Fprintf(os.Stderr, "==> cleaning up partial init of %s\n", ic.name)

	if runner, err := ic.gh.FindRunner(ic.target, ic.name); err == nil && runner != nil {
		fmt.Fprintf(os.Stderr, "    unregistering %s from GitHub (id=%d)\n", ic.name, runner.ID)
		if err := ic.gh.DeleteRunner(ic.target, runner.ID); err != nil {
			fmt.Fprintf(os.Stderr, "    (warn) DeleteRunner: %v\n", err)
		}
	}

	// Host-isolated cleanup runs whether or not state was saved — the runner
	// home dir + downloaded agent tarball may exist from a crash mid-provision,
	// before activate persisted state.
	if ic.kind == macKind && ic.isolation == "host" {
		fmt.Fprintf(os.Stderr, "    destroying host-isolated runner %s\n", ic.name)
		if err := hostmac.Destroy(ic.name); err != nil {
			fmt.Fprintf(os.Stderr, "    (warn) hostmac destroy: %v\n", err)
		}
		_ = state.Remove(ic.name)
		return
	}

	s, _ := state.Load(ic.name)
	if s == nil {
		// VM was never recorded — nothing local to remove.
		return
	}

	if s.EffectiveBackend() == "tart" {
		fmt.Fprintf(os.Stderr, "    destroying tart VM %s\n", ic.name)
		_ = tart.Stop(ic.name, 30)
		if err := tart.Delete(ic.name); err != nil {
			fmt.Fprintf(os.Stderr, "    (warn) tart delete: %v\n", err)
		}
	} else {
		fmt.Fprintf(os.Stderr, "    destroying incus VM %s\n", ic.name)
		if err := incus.Delete(ic.name); err != nil {
			fmt.Fprintf(os.Stderr, "    (warn) incus delete: %v\n", err)
		}
	}

	if err := state.Remove(ic.name); err != nil {
		fmt.Fprintf(os.Stderr, "    (warn) state.Remove: %v\n", err)
	}
}

func kindName(k kind) string {
	switch k {
	case windowsKind:
		return "windows"
	case macKind:
		return "mac"
	default:
		return "linux"
	}
}

func kindPrefix(k kind) string {
	switch k {
	case windowsKind:
		return "win-runner"
	case macKind:
		return "mac-runner"
	default:
		return "linux-runner"
	}
}

// phasesFor returns the TUI phase list for a given (kind, isolation) combo.
// Mac+host has no pull/boot (no VM); other paths share the tart shape on
// macOS or the Incus shape on Linux.
func phasesFor(k kind, isolation string) []tui.PhaseSpec {
	tartPhases := []tui.PhaseSpec{
		{ID: "register", Label: "Register"},
		{ID: "pull", Label: "Pull"},
		{ID: "boot", Label: "Boot"},
		{ID: "activate", Label: "Activate"},
		{ID: "verify", Label: "Verify"},
	}
	switch {
	case k == macKind && isolation == "host":
		return []tui.PhaseSpec{
			{ID: "register", Label: "Register"},
			{ID: "preflight", Label: "Preflight"},
			{ID: "provision", Label: "Provision"},
			{ID: "activate", Label: "Activate"},
			{ID: "verify", Label: "Verify"},
		}
	case k == macKind:
		return tartPhases
	case k == linuxKind && runtime.GOOS == "darwin":
		return tartPhases
	case k == linuxKind:
		return []tui.PhaseSpec{
			{ID: "register", Label: "Register"},
			{ID: "boot", Label: "Boot"},
			{ID: "cloud_init", Label: "Cloud-init"},
			{ID: "verify", Label: "Verify"},
		}
	default: // windowsKind
		return []tui.PhaseSpec{
			{ID: "register", Label: "Register"},
			{ID: "boot", Label: "Boot"},
			{ID: "partition", Label: "Partition"},
			{ID: "activate", Label: "Activate"},
			{ID: "verify", Label: "Verify"},
		}
	}
}

func doInit(r *tui.Runner, ic *initContext) error {
	r.Start("register")
	r.Log("POST /%s/actions/runners/registration-token", ic.target)
	tok, err := ic.gh.RegistrationToken(ic.target)
	if err == nil {
		r.Log("token issued (1h ttl)")
	}
	r.End("register", err)
	if err != nil {
		return err
	}
	ic.regToken = tok

	vars := provision.Vars{
		RepoURL: ic.ownerURL, RegToken: ic.regToken,
		Name: ic.name, Labels: ic.labels,
	}
	switch {
	case ic.kind == macKind && ic.isolation == "host":
		return doInitMacHost(r, ic, vars)
	case ic.kind == macKind:
		return doInitTart(r, ic, vars, macImage, "mac", true)
	case ic.kind == linuxKind && runtime.GOOS == "darwin":
		return doInitTart(r, ic, vars, linuxARMImage, "linux", false)
	case ic.kind == linuxKind:
		return doInitLinux(r, ic, vars)
	default:
		return doInitWindows(r, ic, vars)
	}
}

func doInitLinux(r *tui.Runner, ic *initContext, vars provision.Vars) error {
	userData, err := provision.LinuxCloudInit(vars)
	if err != nil {
		return err
	}
	r.Start("boot")
	if ic.isolation == "container" {
		r.Log("incus launch %s %s", linuxImage, ic.name)
		r.Log("  cpus=4  memory=8GiB  (rootfs grows on demand)")
		err = incus.LaunchContainer(linuxImage, ic.name, map[string]string{
			"user.user-data": userData,
			"limits.cpu":     "4",
			"limits.memory":  "8GiB",
		}, nil)
	} else {
		r.Log("incus launch %s %s --vm", linuxImage, ic.name)
		r.Log("  cpus=4  memory=8GiB  root=75GiB")
		// 75 GiB matches GitHub's ubuntu-latest hosted-runner free disk (~74 GB
		// on a ~84 GB volume), so jobs that assume that headroom won't surprise.
		err = incus.LaunchVM(linuxImage, ic.name, map[string]string{
			"user.user-data":      userData,
			"security.secureboot": "false",
			"limits.cpu":          "4",
			"limits.memory":       "8GiB",
		}, map[string]string{"root.size": "75GiB"})
	}
	if err == nil {
		r.Log("instance started (cloud-init now running async inside the guest)")
		r.Log("writing ~/.krapow/state/%s.json", ic.name)
		err = state.Save(state.Runner{
			Name: ic.name, Kind: "linux", Isolation: ic.isolation,
			Repo: ic.owner, Scope: ic.scope,
			Labels: ic.labels, Created: time.Now(),
		})
	}
	r.End("boot", err)
	if err != nil {
		return err
	}

	r.Start("cloud_init")
	r.Log("waiting for cloud-init (runner agent install)")
	tail := &cloudInitTail{name: ic.name}
	err = waitForLinuxCloudInit(r, ic.name, tail, 30*time.Minute)
	r.End("cloud_init", err)
	if err != nil {
		return err
	}

	r.Start("verify")
	r.Log("polling GitHub for runner to report 'online'")
	err = verifyRunnerOnline(r, ic.gh, ic.target, ic.name, 2*time.Minute)
	r.End("verify", err)
	return err
}

// doInitTart drives the macOS/Linux-ARM-via-Tart path: pull image, clone into a
// per-runner VM, start it detached, SSH-provision the runner agent, verify.
//
// guestKind is what gets persisted in state.Kind ("mac" or "linux"). isMac
// picks between the macOS and Linux-ARM provisioning scripts.
func doInitTart(r *tui.Runner, ic *initContext, vars provision.Vars, image, guestKind string, isMac bool) error {
	r.Start("pull")
	exists, err := tart.ImageExists(image)
	if err != nil {
		r.End("pull", err)
		return err
	}
	if exists {
		r.Log("image %s already in tart cache", image)
	} else {
		r.Log("tart pull %s (first pull can be 30+ GB / many minutes)", image)
		err = tart.Pull(image)
	}
	r.End("pull", err)
	if err != nil {
		return err
	}

	r.Start("boot")
	r.Log("tart clone %s %s", image, ic.name)
	if err := tart.Clone(image, ic.name); err != nil {
		r.End("boot", err)
		return err
	}
	logPath, err := tartLogPath(ic.name)
	if err != nil {
		r.End("boot", err)
		return err
	}
	r.Log("tart run --no-graphics %s (detached; logs: %s)", ic.name, logPath)
	if err := tart.RunDetached(ic.name, logPath); err != nil {
		r.End("boot", err)
		return err
	}
	if err := state.Save(state.Runner{
		Name: ic.name, Kind: guestKind, Backend: "tart", Isolation: ic.isolation,
		Repo: ic.owner, Scope: ic.scope, Labels: ic.labels, Created: time.Now(),
	}); err != nil {
		r.End("boot", err)
		return err
	}
	r.Log("polling tart ip + ssh port 22")
	ip, err := waitForTartSSH(r, ic.name, 5*time.Minute)
	if err == nil {
		r.Log("connected: %s", ip)
	}
	r.End("boot", err)
	if err != nil {
		return err
	}

	r.Start("activate")
	var script string
	if isMac {
		script, err = provision.MacProvision(vars)
	} else {
		script, err = provision.LinuxARMProvision(vars)
	}
	if err != nil {
		r.End("activate", err)
		return err
	}
	r.Log("ssh admin@%s bash -s  (downloading runner, ./config.sh)", ip)
	err = macssh.Provision(ip, script)
	r.End("activate", err)
	if err != nil {
		return err
	}

	r.Start("verify")
	r.Log("polling GitHub for runner to report 'online'")
	err = verifyRunnerOnline(r, ic.gh, ic.target, ic.name, 2*time.Minute)
	r.End("verify", err)
	return err
}

// doInitMacHost drives the macOS host-isolated path: prepare a sandboxed
// HOME under ~/.krapow/runners/<name>/, run the provisioning script as the
// current user against that HOME (downloads the runner agent, ./config.sh),
// install a per-runner LaunchAgent in the user's real LaunchAgents dir, then
// poll GitHub for the runner to report online.
//
// No VM, no sudo, no service user. The HOME override is a hygiene boundary
// against credential/cache pollution — not a security one.
func doInitMacHost(r *tui.Runner, ic *initContext, vars provision.Vars) error {
	r.Start("preflight")
	rh, err := hostmac.RunnerHome(ic.name)
	if err != nil {
		r.End("preflight", err)
		return err
	}
	r.Log("runner home: %s", rh)
	if err := hostmac.PrepareHome(ic.name); err != nil {
		r.End("preflight", err)
		return err
	}
	if _, err := exec.LookPath("gh"); err != nil {
		err = fmt.Errorf("'gh' not on PATH — host-isolated runners use the host's gh (brew install gh)")
		r.End("preflight", err)
		return err
	}
	r.End("preflight", nil)

	r.Start("provision")
	script, err := provision.MacHostProvision(vars)
	if err != nil {
		r.End("provision", err)
		return err
	}
	r.Log("bash -s with HOME=%s (downloading runner, ./config.sh)", rh)
	if err := hostmac.RunProvision(ic.name, script, r.Logger(), r.Logger()); err != nil {
		r.End("provision", err)
		return err
	}
	r.End("provision", nil)

	r.Start("activate")
	plistPath, _ := hostmac.LaunchAgentPath(ic.name)
	r.Log("writing %s", plistPath)
	if err := hostmac.WritePlist(ic.name); err != nil {
		r.End("activate", err)
		return err
	}
	r.Log("launchctl bootstrap user/%d %s", os.Getuid(), filepath.Base(plistPath))
	if err := hostmac.Bootstrap(ic.name); err != nil {
		r.End("activate", err)
		return err
	}
	if err := state.Save(state.Runner{
		Name: ic.name, Kind: "mac", Backend: "host", Isolation: ic.isolation,
		Repo: ic.owner, Scope: ic.scope, Labels: ic.labels, Created: time.Now(),
	}); err != nil {
		r.End("activate", err)
		return err
	}
	r.End("activate", nil)

	r.Start("verify")
	r.Log("polling GitHub for runner to report 'online'")
	err = verifyRunnerOnline(r, ic.gh, ic.target, ic.name, 2*time.Minute)
	r.End("verify", err)
	return err
}

// tartLogPath returns ~/.krapow/logs/<name>.log, ensuring the dir exists.
func tartLogPath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".krapow", "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".log"), nil
}

// waitForTartSSH polls `tart ip` until the guest has a DHCP lease, then waits
// for port 22 to accept connections. Linux ARM images come up faster than
// macOS, so 5 minutes covers both with headroom.
func waitForTartSSH(r *tui.Runner, name string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	announcedIP := false
	for time.Now().Before(deadline) {
		ip, err := tart.IP(name)
		if err == nil && ip != "" {
			if !announcedIP {
				r.Log("IPv4 up: %s", ip)
				announcedIP = true
			}
			if err := macssh.WaitForPort(ip, 30*time.Second); err == nil {
				return ip, nil
			}
		}
		time.Sleep(5 * time.Second)
	}
	return "", fmt.Errorf("timed out waiting for tart VM %s to expose SSH", name)
}

func doInitWindows(r *tui.Runner, ic *initContext, vars provision.Vars) error {
	r.Start("boot")
	rootSize := fmt.Sprintf("%dGiB", ic.diskGiB)
	r.Log("incus launch %s %s --vm", windowsImage, ic.name)
	r.Log("  cpus=4  memory=8GiB  root=%s", rootSize)
	// root.size must be >= the published base image's disk size (60 GiB — set
	// in the bake VM). Cloning into a smaller volume fails with "Source image
	// size exceeds specified volume size". The floor is enforced in
	// runInitWin so a bad --disk is rejected before we get here.
	err := incus.LaunchVM(windowsImage, ic.name, map[string]string{
		"security.secureboot": "false",
		"limits.cpu":          "4",
		"limits.memory":       "8GiB",
	}, map[string]string{"root.size": rootSize})
	if err == nil {
		r.Log("VM started; writing state file")
		err = state.Save(state.Runner{
			Name: ic.name, Kind: "windows", Isolation: ic.isolation,
			Repo: ic.owner, Scope: ic.scope,
			Labels: ic.labels, Created: time.Now(),
		})
	}
	if err != nil {
		r.End("boot", err)
		return err
	}

	r.Log("polling DHCP for IPv4 + SSH port 22 (Windows boot ~3-5 min)")
	ip, err := waitForWindowsSSHLogged(r, ic.name, 15*time.Minute)
	if err == nil {
		r.Log("connected: %s", ip)
	}
	r.End("boot", err)
	if err != nil {
		return err
	}
	ic.vmIP = ip

	privPath, _, err := sshkeys.EnsureKeyPair()
	if err != nil {
		return err
	}
	c, err := winssh.Dial(ip, privPath, 30*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()

	r.Start("partition")
	r.Log("Resize-Partition C: to fill the disk")
	_, err = c.RunPowerShell(`
$max = (Get-PartitionSupportedSize -DriveLetter C).SizeMax
$cur = (Get-Partition -DriveLetter C).Size
if ($max -gt $cur) {
    Resize-Partition -DriveLetter C -Size $max
    Write-Host ("C: grown to {0:N1} GiB" -f ($max / 1GB))
} else {
    Write-Host ("C: already at max ({0:N1} GiB)" -f ($cur / 1GB))
}
`)
	r.End("partition", err)
	if err != nil {
		return fmt.Errorf("resize C: failed: %w", err)
	}

	ps1, err := provision.WindowsPS1(vars)
	if err != nil {
		return err
	}
	r.Start("activate")
	r.Log("downloading actions-runner-win-x64 release & running config.cmd")
	_, err = c.RunPowerShell(ps1)
	r.End("activate", err)
	if err != nil {
		return err
	}

	r.Start("verify")
	r.Log("polling GitHub for runner to report 'online'")
	err = verifyRunnerOnline(r, ic.gh, ic.target, ic.name, 2*time.Minute)
	r.End("verify", err)
	return err
}

// verifyRunnerOnline polls GitHub until the runner is registered AND its
// status is 'online' (heartbeating).
func verifyRunnerOnline(r *tui.Runner, gh *githubapi.Client, target, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		runner, err := gh.FindRunner(target, name)
		if err != nil {
			return err
		}
		if runner == nil {
			r.Log("not in GitHub's runner list yet")
		} else if runner.Status != "online" {
			r.Log("registered (id=%d) but status=%s; waiting for heartbeat", runner.ID, runner.Status)
		} else {
			r.Log("online (id=%d, busy=%v)", runner.ID, runner.Busy)
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("runner %s never reported 'online' within %s", name, timeout)
}

// cloudInitTail tracks how far we've streamed cloud-init-output.log so
// successive phases don't replay the same bytes into the viewport.
type cloudInitTail struct {
	name   string
	offset int64
}

func (t *cloudInitTail) stream(r *tui.Runner) {
	tailCmd := exec.Command("incus", "exec", t.name, "--",
		"sh", "-c", fmt.Sprintf("tail -c +%d /var/log/cloud-init-output.log 2>/dev/null || true", t.offset+1))
	newBytes, err := tailCmd.Output()
	if err != nil || len(newBytes) == 0 {
		return
	}
	t.offset += int64(len(newBytes))
	for _, line := range strings.Split(string(newBytes), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		r.Log("%s", line)
	}
}

func waitForLinuxCloudInit(r *tui.Runner, name string, tail *cloudInitTail, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		statusOut, _ := exec.Command("incus", "exec", name, "--", "cloud-init", "status").Output()
		status := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(statusOut)), "status:"))

		tail.stream(r)

		switch status {
		case "done":
			return nil
		case "error":
			return fmt.Errorf("cloud-init failed in %s (check `incus exec %s -- cloud-init status --long`)", name, name)
		}
		time.Sleep(8 * time.Second)
	}
	return fmt.Errorf("timed out waiting for cloud-init in %s", name)
}

func waitForWindowsSSHLogged(r *tui.Runner, name string, timeout time.Duration) (string, error) {
	privPath, _, err := sshkeys.EnsureKeyPair()
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(timeout)
	announcedIP := false
	attempt := 0
	for time.Now().Before(deadline) {
		ip := vmIPv4(name)
		if ip != "" {
			if !announcedIP {
				r.Log("IPv4 up: %s", ip)
				announcedIP = true
			}
			attempt++
			r.Log("ssh attempt %d on %s:22", attempt, ip)
			c, err := winssh.Dial(ip, privPath, 20*time.Second)
			if err == nil {
				_ = c.Close()
				return ip, nil
			}
		}
		time.Sleep(15 * time.Second)
	}
	return "", fmt.Errorf("timed out waiting for SSH on %s", name)
}

func vmIPv4(name string) string {
	out, err := exec.Command("incus", "list", name, "--format", "csv", "-c", "4").Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(out))
	if i := strings.Index(line, " "); i > 0 {
		return line[:i]
	}
	return line
}
