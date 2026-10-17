package cmd

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qubelylabs/origamy-cli/internal/ui"
)

var uninstallCmd = &cobra.Command{
	Use:   "uninstall [data-plane-id]",
	Short: "Tear down an Origamy data plane from this machine",
	Long: `Remove the Origamy data plane this CLI deployed.

Finds the install to remove (override with --target):
  • Kubernetes → helm uninstall odp + delete the origamy-dp namespace, when the
                 cluster kubectl points at has the release or the namespace
  • Docker     → docker compose down -v in ./origamy-dp-<id>/, pinned to that
                 project's name (COMPOSE_PROJECT_NAME in its .env, else the
                 top-level name: of its docker-compose.yml, else the
                 directory's name; never the shell's)

On Docker, the project's name is a name compose gives containers and volumes,
and "down -v" removes every container and volume that carries it. It is
refused while a container of that name was started from another directory
that still exists. A container whose directory is gone is taken for this
plane's (a project that was moved): it is removed with the rest, and so is
every volume labelled with the name, those the compose file does not declare
too. When the plane's data volumes exist under another name than the pinned
one (a COMPOSE_PROJECT_NAME exported in the shell when it ran, and no longer),
"down -v" cannot reach them: uninstall says which they are, removes the
directory (and the .env they were created with) only once you confirm, and
does not report their data as removed.

Run this BEFORE deactivating the workspace in your dashboard so the plane
stops trying to reconnect.`,
	Example: `  origamy uninstall dp-13-qaglj0ye
  origamy uninstall --target kubernetes`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := ""
		if len(args) > 0 {
			id = args[0]
		}
		yes, _ := cmd.Flags().GetBool("yes")
		target, _ := cmd.Flags().GetString("target")
		return runUninstall(id, target, yes)
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	uninstallCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
	uninstallCmd.Flags().String("target", string(targetAuto), "Which install to remove: auto, kubernetes, or docker")
}

func runUninstall(id, target string, assumeYes bool) error {
	ui.Title("Uninstall Origamy data plane")
	if id != "" {
		ui.KV("Data plane", ui.Bold(id))
	}

	// Unlike deploy, a reachable cluster alone is not enough to go the
	// Kubernetes way: on a Docker host whose kubeconfig still points at some
	// cluster, `origamy uninstall <id>` must tear down ./origamy-dp-<id>, not
	// delete a namespace elsewhere. Kubernetes is chosen only when that cluster
	// actually holds the release or the namespace.
	reachable := strings.ToLower(strings.TrimSpace(target)) != string(targetDocker) && hasKubernetes()
	k8s := reachable && (releaseInstalled() || namespaceExists(namespace))
	docker := strings.ToLower(strings.TrimSpace(target)) != string(targetKubernetes) && hasDocker()
	where, err := resolveTarget(target, k8s, docker)
	if err != nil {
		if reachable && !k8s && (errors.Is(err, errNoTarget) || strings.Contains(err.Error(), "Kubernetes")) {
			ctx := kubeContext()
			return fail(fmt.Sprintf("Nothing to uninstall in the cluster kubectl points at (context %s): no '%s' release and no '%s' namespace.", orDash(ctx), release, namespace),
				"If the data plane runs on Docker here, pass its id: origamy uninstall <data-plane-id>")
		}
		if errors.Is(err, errNoTarget) {
			return fail("No Origamy data plane found on this machine.",
				"Nothing to uninstall here — run this command where the data plane is deployed.")
		}
		return fail(err.Error(), "Pass --target kubernetes or --target docker, or omit it to auto-detect.")
	}
	if where == targetKubernetes {
		return uninstallKubernetes(assumeYes)
	}
	if id == "" {
		return fail("Docker detected, but no data plane id was given.",
			"Run: origamy uninstall <data-plane-id>  (the id is on your dashboard's Connections page).")
	}
	return uninstallDocker(id, assumeYes)
}

// confirmTeardown asks for an explicit "yes" before destroying anything, unless
// --yes was passed.
func confirmTeardown(what string, assumeYes bool) bool {
	if assumeYes {
		return true
	}
	ui.Warn("This permanently removes %s.", what)
	ans := promptString("Type 'yes' to continue")
	return ans == "yes" || ans == "y"
}

// ── Kubernetes ────────────────────────────────────────────────────────────────

func uninstallKubernetes(assumeYes bool) error {
	if _, err := exec.LookPath("helm"); err != nil {
		return fail("Helm is required to uninstall a Kubernetes data plane.",
			"Install it from https://helm.sh/docs/intro/install/ and retry.")
	}
	ui.Title("Target")
	if ctx := kubeContext(); ctx != "" {
		ui.Success("Kubernetes cluster detected (kubectl context: %s)", ui.Bold(ctx))
	} else {
		ui.Success("Kubernetes cluster detected")
	}

	if !confirmTeardown(fmt.Sprintf("the '%s' release and the '%s' namespace", release, namespace), assumeYes) {
		return fail("Cancelled.", "")
	}

	// Uninstall the release. A missing release isn't fatal — we still want to
	// drop the namespace — so we don't bail here.
	sp := ui.Start("Uninstalling Helm release %s", release)
	if out, err := runCaptured("helm", "uninstall", release, "--namespace", namespace); err != nil {
		sp.Fail("Helm release not removed (it may already be gone)")
		if tail := lastLines(out, 3); tail != "" {
			ui.Detail("%s", ui.Dim(tail))
		}
	} else {
		sp.Success("Helm release %s removed", release)
	}

	sp = ui.Start("Deleting namespace %s", namespace)
	if _, err := runCaptured("kubectl", "delete", "namespace", namespace, "--ignore-not-found"); err != nil {
		sp.Fail("Could not delete namespace %s", namespace)
		return fail("Namespace deletion failed.",
			fmt.Sprintf("Remove it manually: kubectl delete namespace %s", namespace))
	}
	sp.Success("Namespace %s deleted", namespace)

	ui.Title("Done")
	ui.Success("Data plane uninstalled. You can now deactivate the workspace in your dashboard.")
	return nil
}

// ── Docker ────────────────────────────────────────────────────────────────────

func uninstallDocker(id string, assumeYes bool) error {
	dir := "origamy-dp-" + id
	compose := filepath.Join(dir, "docker-compose.yml")
	if _, err := os.Stat(compose); err != nil {
		return fail(fmt.Sprintf("No data plane found at ./%s", dir),
			"Run this from the directory where you deployed, or pass the right data plane id.")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fail("Could not read the working directory.", err.Error())
	}
	envPath := filepath.Join(abs, ".env")
	ui.Title("Target")
	ui.Success("Docker deployment detected (./%s)", dir)
	// Not while an upgrade or a deploy works on it (lock.go).
	unlock, err := lockProject(dir, "origamy uninstall")
	if err != nil {
		return lockFail(err, "Nothing has been removed.")
	}
	defer unlock()

	// Only this plane's own containers and volumes go. `down -v` works on a
	// compose project by name: the name is the one the CLI pins for the
	// project (composeProjectName; never a COMPOSE_PROJECT_NAME exported in
	// the shell, which can name another project), and every container that
	// carries it must have been started from this directory.
	name := composeProjectName(abs, envPath)
	nothing := "Nothing has been removed."
	o, rows, ok := composeOwners(abs, name)
	switch {
	case !ok:
		return fail("Docker could not list the containers on this host, so whose they are could not be checked.",
			outputHint(rows)+"Fix the cause, then rerun `origamy uninstall "+id+"`. "+nothing)
	case len(o.elsewhere) > 0:
		var dirs []string
		for _, d := range o.elsewhere {
			dirs = append(dirs, cmp.Or(d, "a directory its containers do not name"))
		}
		return fail(fmt.Sprintf("The compose project %s, this data plane's, also has containers started from %s: `docker compose down -v` would remove them, and their volumes, too.", name, strings.Join(dirs, ", ")),
			"If they are this plane's (a copy of ./"+dir+" runs them), uninstall from there. Otherwise remove them yourself first, or give this plane its own name (COMPOSE_PROJECT_NAME=<the name its containers run under> in "+envPath+"), then rerun. "+nothing)
	case len(o.here) > 0:
		return fail(fmt.Sprintf("Containers of this data plane run under the compose project name %s, which this CLI does not pin for ./%s (it pins %s): a COMPOSE_PROJECT_NAME exported in the shell when they were started.", strings.Join(o.here, ", "), dir, name),
			"Put the name they run under in "+envPath+" (COMPOSE_PROJECT_NAME="+o.here[0]+"), then rerun `origamy uninstall "+id+"`. "+nothing)
	}

	// The plane's data can sit under another project name than the one pinned
	// here, taken down (no container): `down -v` cannot reach it.
	var stranded map[string][]string
	if !o.pinned {
		if mine, others, ok := planeVolumes(abs, name, rows); ok && len(mine) == 0 && len(others) > 0 {
			stranded = others
			list, suggest := otherVolumesList(others)
			ui.Warn("Nothing of this data plane exists under the compose project name %s, but the data volumes of a plane like it do under another: %s.", name, list)
			ui.Detail("`down -v` for %s cannot remove them, and they stay on this host when ./%s goes, with the passwords in its .env that they were created with.", name, dir)
			ui.Detail("If they are this plane's (it ran with COMPOSE_PROJECT_NAME exported in the shell), cancel, put COMPOSE_PROJECT_NAME=%s in %s, and rerun: uninstall then removes them.", suggest, envPath)
		}
	}
	what := fmt.Sprintf("the containers and volumes of compose project %s in ./%s", name, dir)
	if stranded != nil {
		what = fmt.Sprintf("./%s with the .env in it (nothing is left of compose project %s; the volumes named above stay)", dir, name)
	}
	if !confirmTeardown(what, assumeYes) {
		return fail("Cancelled.", "")
	}

	sp := ui.Start("Stopping and removing containers + volumes")
	if out, err := dockerRun(composeEnv(dotenvValue(envPath, "DP_IMAGE_TAG")), "docker", composeCmd(abs, envPath, "down", "-v", "--remove-orphans")...); err != nil {
		sp.Fail("docker compose down failed")
		return diagnoseDocker(out, "rerun `origamy uninstall "+id+"`")
	}
	if stranded != nil {
		sp.Warn("Compose project %s had no containers or volumes to remove", name)
	} else {
		sp.Success("Containers and volumes of %s removed", name)
	}

	// Best-effort: remove the generated deploy directory (.env, compose, etc.),
	// lock file included; Windows removes that only once it is unlocked.
	err = os.RemoveAll(dir)
	if unlock(); err != nil {
		err = os.RemoveAll(dir)
	}
	if err == nil {
		ui.Success("Removed ./%s", dir)
	}

	ui.Title("Done")
	if stranded != nil {
		list, _ := otherVolumesList(stranded)
		var names []string
		for _, p := range slices.Sorted(maps.Keys(stranded)) {
			names = append(names, stranded[p]...)
		}
		ui.Warn("The data plane's directory is removed, but its data was NOT: these volumes stay on this host: %s.", list)
		ui.Detail("Remove them yourself once you no longer need the data: docker volume rm %s", strings.Join(names, " "))
		ui.Success("Nothing of the data plane runs from ./%s any more. You can now deactivate the workspace in your dashboard.", dir)
		return nil
	}
	ui.Success("Data plane uninstalled. You can now deactivate the workspace in your dashboard.")
	return nil
}
