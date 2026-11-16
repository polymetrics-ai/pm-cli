package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// container is one identity provider container that the spike owns. It runs
// through the job's engine socket, like the stack services.
type container struct {
	name     string
	image    string
	logPath  string
	stopLogs context.CancelFunc
	logsDone chan struct{}
}

func docker(ctx context.Context, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "docker", args...)
	command.Env = dockerEnvironment()
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if len(message) > 300 {
			message = message[:300]
		}
		return stdout.String(), fmt.Errorf("docker %s: %s", args[0], report.scrub(message))
	}
	return stdout.String(), nil
}

// dockerEnvironment passes only the engine endpoint and the private Docker
// configuration that with-stack-fixture.py gives to its child.
func dockerEnvironment() []string {
	environment := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	for _, name := range []string{"DOCKER_HOST", "DOCKER_CONFIG"} {
		if value := os.Getenv(name); value != "" {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}

// pull tries each reference in order and returns the first one that the
// engine pulled, with its repository digest and size.
func pull(id string, references []string) (string, string, string, bool) {
	for _, reference := range references {
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		_, err := docker(ctx, "pull", "--quiet", reference)
		cancel()
		if err != nil {
			report.INFO(id+".pull", "%s: not pulled (%v)", reference, err)
			continue
		}
		pullTime := elapsed(start)
		ctx, cancel = context.WithTimeout(context.Background(), time.Minute)
		output, err := docker(ctx, "image", "inspect", "--format", "{{json .RepoDigests}} {{.Size}}", reference)
		cancel()
		if err != nil {
			report.INFO(id+".pull", "%s: pulled, inspect failed", reference)
			return reference, "", pullTime, true
		}
		report.INFO(id+".image", "%s pulled in %s; repo digests and size: %s", reference, pullTime, strings.TrimSpace(output))
		return reference, strings.TrimSpace(output), pullTime, true
	}
	return "", "", "", false
}

// start runs the container detached and follows its log until a line matches
// ready, the container exits or the limit passes. It is one wait with one
// limit for one product event: the provider's own ready line.
func (c *container) start(id string, args []string, ready *regexp.Regexp, limit time.Duration) (time.Duration, error) {
	begin := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	runArgs := append([]string{"run", "--detach", "--name", c.name, "--label", "ai.polymetrics.pm-login-spike=" + c.name}, args...)
	_, err := docker(ctx, runArgs...)
	cancel()
	if err != nil {
		return 0, err
	}
	logFile, err := os.OpenFile(c.logPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, errors.New("create the container log")
	}
	followCtx, stopFollow := context.WithCancel(context.Background())
	c.stopLogs = stopFollow
	c.logsDone = make(chan struct{})
	command := exec.CommandContext(followCtx, "docker", "logs", "--follow", c.name)
	command.Env = dockerEnvironment()
	pipeReader, pipeWriter, err := os.Pipe()
	if err != nil {
		_ = logFile.Close()
		return 0, errors.New("open the container log pipe")
	}
	command.Stdout = pipeWriter
	command.Stderr = pipeWriter
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		_ = pipeReader.Close()
		_ = pipeWriter.Close()
		return 0, errors.New("follow the container log")
	}
	_ = pipeWriter.Close()
	found := make(chan time.Duration, 1)
	go func() {
		defer close(c.logsDone)
		defer logFile.Close()
		defer pipeReader.Close()
		scanner := bufio.NewScanner(pipeReader)
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		matched := false
		for scanner.Scan() {
			line := scanner.Text()
			_, _ = logFile.WriteString(line + "\n")
			if !matched && ready.MatchString(line) {
				matched = true
				found <- time.Since(begin)
			}
		}
		if !matched {
			close(found)
		}
		_ = command.Wait()
	}()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case duration, ok := <-found:
		if ok {
			return duration, nil
		}
		return 0, errors.New("the container log ended before the ready line")
	case <-timer.C:
		return 0, fmt.Errorf("no ready line within %s", limit)
	}
}

func (c *container) stats(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := docker(ctx, "stats", "--no-stream", "--format", "{{.MemUsage}} cpu={{.CPUPerc}} pids={{.PIDs}}", c.name)
	if err != nil {
		report.INFO(id+".resources", "stats unavailable: %v", err)
		return
	}
	report.INFO(id+".resources", "memory and cpu now: %s", strings.TrimSpace(output))
}

func (c *container) remove(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := docker(ctx, "rm", "--force", "--volumes", c.name)
	if c.stopLogs != nil {
		select {
		case <-c.logsDone:
		case <-time.After(10 * time.Second):
			c.stopLogs()
			<-c.logsDone
		}
	}
	if err != nil {
		report.FAIL(id+".cleanup", "container removal failed: %v", err)
		return
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	output, _ := docker(ctx2, "ps", "--all", "--quiet", "--filter", "name=^"+c.name+"$")
	report.check(id+".cleanup", strings.TrimSpace(output) == "", "the identity provider container is removed")
}
