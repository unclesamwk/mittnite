package proc

import (
	"context"
	"errors"
	"fmt"
	"github.com/mittwald/mittnite/internal/config"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
)

func (job *CommonJob) Init() {
	for w := range job.Config.Watches {
		watch := &job.Config.Watches[w]
		job.watchingFiles = make(map[string]time.Time)

		paths, err := filepath.Glob(watch.Filename)
		if err != nil {
			continue
		}

		for _, p := range paths {
			stat, err := os.Stat(p)
			if err != nil {
				continue
			}

			job.watchingFiles[p] = stat.ModTime()
		}
	}
}

func (job *CommonJob) Run(ctx context.Context, _ chan<- error) error {
	l := log.WithField("job.name", job.Config.Name)

	attempts := 0
	maxAttempts := job.Config.MaxAttempts

	if maxAttempts <= 0 {
		maxAttempts = 3
	}

	for { // restart failed jobs as long mittnite is running
		err := job.startOnce(ctx, nil)
		switch err {
		case nil:
			if job.Config.OneTime {
				l.Info("one-time job has ended successfully")
				return nil
			}
			l.Warn("job exited without errors")
		case ProcessWillBeRestartedError:
			l.Info("restart process")
			continue
		default:
			l.WithError(err).Error("job exited with error")
		}

		attempts++
		if attempts < maxAttempts {
			l.WithField("job.maxAttempts", maxAttempts).WithField("job.usedAttempts", attempts).Info("remaining attempts")
			continue
		}

		if job.Config.CanFail {
			l.WithField("job.maxAttempts", maxAttempts).Warn("reached max retries")
			return nil
		}

		return fmt.Errorf("reached max retries for job %s", job.Config.Name)
	}
}

func (job *CommonJob) executeWatchCommand(watchCmd *config.WatchCommand) error {
	if len(watchCmd.Command) == 0 {
		return errors.New("command is missing")
	}
	cmd := exec.Command(watchCmd.Command, watchCmd.Args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()

	if watchCmd.Env != nil {
		cmd.Env = append(cmd.Env, watchCmd.Env...)
	}

	log.WithField("job.name", job.Config.Name).
		Info("executing watch command")
	return cmd.Run()
}

func (job *CommonJob) Watch() {
	for w := range job.Config.Watches {
		watch := &job.Config.Watches[w]
		signal := false
		paths, err := filepath.Glob(watch.Filename)
		if err != nil {
			log.Warnf("failed to watch %s: %s", watch.Filename, err.Error())
			continue
		}

		// check existing files
		for _, p := range paths {
			stat, err := os.Stat(p)
			if err != nil {
				continue
			}

			mtime := stat.ModTime()
			if mtime.Equal(job.watchingFiles[p]) {
				continue
			}

			log.Infof("file %s changed, signalling process %s", p, job.Config.Name)
			job.watchingFiles[p] = mtime
			signal = true
		}

		// check deleted files
		for p := range job.watchingFiles {
			_, err := os.Stat(p)
			if os.IsNotExist(err) {
				log.Infof("file %s changed, signalling process %s", p, job.Config.Name)
				delete(job.watchingFiles, p)
				signal = true
			}
		}

		if !signal {
			continue
		}

		l := log.WithField("job.name", job.Config.Name)
		if watch.PreCommand != nil {
			if err := job.executeWatchCommand(watch.PreCommand); err != nil {
				l.WithError(err).Warn("failed to execute pre watch command")
			}
		}

		if watch.Restart {
			job.MarkForRestart()
		}
		job.Signal(syscall.Signal(watch.Signal))

		if watch.PostCommand != nil {
			if err := job.executeWatchCommand(watch.PostCommand); err != nil {
				l.WithError(err).Warn("failed to execute post watch command")
			}
		}
	}
}
