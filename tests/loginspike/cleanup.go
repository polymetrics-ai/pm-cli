package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

const spikeLabel = "ai.polymetrics.pm-login-spike"

// previousSpikeImages are the spike CI image tags of earlier spike runs. Run 1
// (98c363c) built localhost/pmcli-ci:94299bba324a71d55e10d959c22955bc.
var previousSpikeImages = []string{"localhost/pmcli-ci:94299bba324a71d55e10d959c22955bc"}

// show runs one engine command through the job's engine socket and prints the
// command, each output line and the exit result.
func show(id string, args ...string) (string, error) {
	report.INFO(id, "$ docker %s", strings.Join(args, " "))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	output, err := docker(ctx, args...)
	for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		if line != "" {
			report.INFO(id, "  %s", line)
		}
	}
	if err != nil {
		report.INFO(id, "  exit: error: %v", err)
	} else {
		report.INFO(id, "  exit: 0")
	}
	return output, err
}

func ownImage() string {
	name := fmt.Sprintf("pmcli-%s-%s-bdd", os.Getenv("GITHUB_RUN_ID"), os.Getenv("GITHUB_RUN_ATTEMPT"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := docker(ctx, "inspect", "--format", "{{.Config.Image}}", name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(output)
}

func spikeImageReferences() []string {
	references := []string{}
	for _, candidate := range candidates {
		references = append(references, candidate.references...)
	}
	return references
}

// listSpikeResources lists the labelled containers and the spike images.
func listSpikeResources(id, own string) {
	show(id, "ps", "--all", "--filter", "label="+spikeLabel, "--format", "{{.ID}} {{.Names}} {{.Image}} {{.Status}}")
	references := append(spikeImageReferences(), previousSpikeImages...)
	if own != "" {
		references = append(references, own)
	}
	for _, reference := range references {
		show(id, "image", "inspect", "--format", "{{.Id}} {{.RepoTags}} {{.Size}}", reference)
	}
}

// runnerCleanup is the last step of the last spike run: the runner cleanup
// that the review asked for. It lists, removes and lists again the labelled
// containers and the spike images on the runner engine that this job uses. It
// never uses a force image removal: that would also remove the running job
// container.
func runnerCleanup() {
	own := ownImage()
	report.INFO("cleanup.runner", "runner %s; this job's CI image %q", os.Getenv("RUNNER_NAME"), own)
	listSpikeResources("cleanup.runner.before", own)
	output, _ := docker(context.Background(), "ps", "--all", "--quiet", "--filter", "label="+spikeLabel)
	for _, container := range strings.Fields(output) {
		show("cleanup.runner.remove", "rm", "--force", "--volumes", container)
	}
	for _, reference := range spikeImageReferences() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := docker(ctx, "image", "inspect", "--format", "{{.Id}}", reference)
		cancel()
		if err == nil {
			show("cleanup.runner.remove", "image", "rm", reference)
		}
	}
	for _, reference := range previousSpikeImages {
		if reference == own {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := docker(ctx, "image", "inspect", "--format", "{{.Id}}", reference)
		cancel()
		if err == nil {
			show("cleanup.runner.remove", "image", "rm", reference)
		}
	}
	if own != "" {
		report.INFO("cleanup.runner.keep", "not removed from inside the job: %s. The running job container uses it, a force removal would stop this job, and the workflow cleanup step starts its container from the same image. Remove it on the runner after the job.", own)
	}
	listSpikeResources("cleanup.runner.after", own)
	remaining, _ := docker(context.Background(), "ps", "--all", "--quiet", "--filter", "label="+spikeLabel)
	report.check("cleanup.runner.containers", strings.TrimSpace(remaining) == "", "no container labelled %s is left on this runner engine", spikeLabel)
	left := 0
	for _, reference := range append(spikeImageReferences(), previousSpikeImages...) {
		if reference == own {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := docker(ctx, "image", "inspect", "--format", "{{.Id}}", reference)
		cancel()
		if err == nil {
			left++
		}
	}
	report.check("cleanup.runner.images", left == 0, "no Dex, Keycloak or earlier spike CI image is left on this runner engine (left %d)", left)
}
