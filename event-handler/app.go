/*
Copyright 2015-2021 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"time"

	"github.com/gravitational/teleport-plugins/lib"
	"github.com/gravitational/teleport-plugins/lib/backoff"
	"github.com/gravitational/teleport-plugins/lib/logger"
	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/sirupsen/logrus"
	log "github.com/sirupsen/logrus"
)

// session is the utility struct used for session ingestion
type session struct {
	// ID current ID
	ID string

	// Index current event index
	Index int64
}

// App is the app structure
type App struct {
	// mainJob is the main poller loop
	mainJob lib.ServiceJob

	// fluentd is an instance of Fluentd client
	fluentd *FluentdClient

	// teleport is an instance of Teleport client
	teleport *TeleportEventsWatcher

	// state is current persisted state
	state *State

	// cmd is start command CLI config
	config *StartCmdConfig

	// semaphore limiter semaphore
	semaphore chan struct{}

	// sessionIDs id queue
	sessions chan session

	// sessionConsumerJob controls session ingestion
	sessionConsumerJob lib.ServiceJob

	// Process
	*lib.Process
}

const (
	// sessionBackoffBase is an initial (minimum) backoff value.
	sessionBackoffBase = 3 * time.Second
	// sessionBackoffMax is a backoff threshold
	sessionBackoffMax = 2 * time.Minute
	// sessionBackoffNumTries is the maximum number of backoff tries
	sessionBackoffNumTries = 5
)

// NewApp creates new app instance
func NewApp(c *StartCmdConfig) (*App, error) {
	app := &App{config: c}
	app.mainJob = lib.NewServiceJob(app.run)
	app.sessionConsumerJob = lib.NewServiceJob(app.runSessionConsumer)
	app.semaphore = make(chan struct{}, c.Concurrency)
	app.sessions = make(chan session)

	return app, nil
}

// Run initializes and runs a watcher and a callback server
func (a *App) Run(ctx context.Context) error {
	a.Process = lib.NewProcess(ctx)

	a.SpawnCriticalJob(a.mainJob)
	a.SpawnCriticalJob(a.sessionConsumerJob)
	<-a.Process.Done()

	return a.Err()
}

// Err returns the error app finished with.
func (a *App) Err() error {
	return trace.NewAggregate(a.mainJob.Err(), a.sessionConsumerJob.Err())
}

// WaitReady waits for http and watcher service to start up.
func (a *App) WaitReady(ctx context.Context) (bool, error) {
	mainReady, err := a.mainJob.WaitReady(ctx)
	if err != nil {
		return false, trace.Wrap(err)
	}

	sessionConsumerReady, err := a.sessionConsumerJob.WaitReady(ctx)
	if err != nil {
		return false, trace.Wrap(err)
	}

	return mainReady && sessionConsumerReady, nil
}

// consumeSession ingests session
func (a *App) consumeSession(ctx context.Context, s session) (bool, error) {
	log := logger.Get(ctx)

	url := a.config.FluentdSessionURL + "." + s.ID + ".log"
	ctx = a.contextWithCancelOnTerminate(ctx)

	log.WithField("id", s.ID).WithField("index", s.Index).Info("Started session events ingest")
	chEvt, chErr := a.teleport.StreamSessionEvents(ctx, s.ID, s.Index)

Loop:
	for {
		select {
		case err := <-chErr:
			return true, trace.Wrap(err)

		case evt := <-chEvt:
			if evt == nil {
				log.WithField("id", s.ID).Info("Finished session events ingest")
				break Loop // Break the main loop
			}

			e, err := NewTeleportEvent(evt, "")
			if err != nil {
				return false, trace.Wrap(err)
			}

			_, ok := a.config.SkipSessionTypes[e.Type]
			if !ok {
				err := a.sendEvent(ctx, url, &e)

				if err != nil && trace.IsConnectionProblem(err) {
					return true, trace.Wrap(err)
				}
				if err != nil {
					return false, trace.Wrap(err)
				}
			}

			// Set session index
			err = a.state.SetSessionIndex(s.ID, e.Index)
			if err != nil {
				return true, trace.Wrap(err)
			}
		case <-ctx.Done():
			if lib.IsCanceled(ctx.Err()) {
				return false, nil
			}

			return false, trace.Wrap(ctx.Err())
		}
	}

	// We finished ingestion and do not need session state anymore
	err := a.state.RemoveSession(s.ID)
	if err != nil {
		return false, trace.Wrap(err)
	}

	return false, nil
}

// runSessionConsumer runs session consuming process
func (a *App) runSessionConsumer(ctx context.Context) error {
	log := logger.Get(ctx)

	a.sessionConsumerJob.SetReady(true)

	ctx = a.contextWithCancelOnTerminate(ctx)

	for {
		select {
		case s := <-a.sessions:
			a.takeSemaphore(ctx)

			log.WithField("id", s.ID).WithField("index", s.Index).Info("Starting session ingest")

			func(s session) {
				a.SpawnCritical(func(ctx context.Context) error {
					defer a.releaseSemaphore(ctx)

					backoff := backoff.NewDecorr(sessionBackoffBase, sessionBackoffMax, clockwork.NewRealClock())
					backoffCount := sessionBackoffNumTries
					log := logger.Get(ctx).WithField("id", s.ID).WithField("index", s.Index)

					for {
						retry, err := a.consumeSession(ctx, s)

						// If sessions needs to retry
						if err != nil && retry {
							log.WithField("err", err).WithField("n", backoffCount).Error("Session ingestion error, retrying")

							// Sleep for required interval
							err := backoff.Do(ctx)
							if err != nil {
								return trace.Wrap(err)
							}

							// Check if there are number of tries left
							backoffCount--
							if backoffCount < 0 {
								log.WithField("err", err).Error("Session ingestion failed")
								return nil
							}
							continue
						}

						if err != nil {
							if !lib.IsCanceled(err) {
								log.WithField("err", err).Error("Session ingestion failed")
							}
							return err
						}

						// No errors, we've finished with this session
						return nil
					}
				})
			}(s)
		case <-ctx.Done():
			if lib.IsCanceled(ctx.Err()) {
				return nil
			}
			return ctx.Err()
		}
	}
}

// run is the main process
func (a *App) run(ctx context.Context) error {
	log := logger.Get(ctx)

	log.WithField("version", Version).WithField("sha", Sha).Printf("Teleport event handler")

	err := a.init(ctx)
	if err != nil {
		return trace.Wrap(err)
	}

	a.restartPausedSessions()

	a.mainJob.SetReady(true)

	ctx = a.contextWithCancelOnTerminate(ctx)

	for {
		err := a.poll(ctx)

		switch {
		case trace.IsConnectionProblem(err):
			log.WithError(err).Error("Failed to connect to Teleport Auth server. Reconnecting...")
		case trace.IsEOF(err):
			log.WithError(err).Error("Watcher stream closed. Reconnecting...")
		case lib.IsCanceled(err):
			log.Debug("Watcher context is cancelled")
			a.Terminate()
			return nil
		default:
			a.Terminate()
			if err == nil {
				return nil
			}
			log.WithError(err).Error("Watcher event loop failed")
			return trace.Wrap(err)
		}
	}
}

// restartPausedSessions restarts sessions saved in state
func (a *App) restartPausedSessions() error {
	sessions, err := a.state.GetSessions()
	if err != nil {
		return trace.Wrap(err)
	}

	if len(sessions) == 0 {
		return nil
	}

	for id, idx := range sessions {
		func(id string, idx int64) {
			a.SpawnCritical(func(ctx context.Context) error {
				ctx = a.contextWithCancelOnTerminate(ctx)

				log.WithField("id", id).WithField("index", idx).Info("Restarting session ingestion")

				s := session{ID: id, Index: idx}

				select {
				case a.sessions <- s:
					return nil
				case <-ctx.Done():
					if lib.IsCanceled(ctx.Err()) {
						return nil
					}

					return ctx.Err()
				}
			})
		}(id, idx)
	}

	return nil
}

// startSessionPoll starts session event ingestion
func (a *App) startSessionPoll(ctx context.Context, e *TeleportEvent) error {
	err := a.state.SetSessionIndex(e.SessionID, 0)
	if err != nil {
		return trace.Wrap(err)
	}

	s := session{ID: e.SessionID, Index: 0}

	select {
	case a.sessions <- s:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// poll polls main audit log
func (a *App) poll(ctx context.Context) error {
	evtCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	chEvt, chErr := a.teleport.Events(evtCtx)

	for {
		select {
		case err := <-chErr:
			log.WithField("err", err).Error("Error ingesting Audit Log")
			return trace.Wrap(err)

		case evt := <-chEvt:
			if evt == nil {
				return nil
			}

			err := a.sendEvent(ctx, a.config.FluentdURL, evt)
			if err != nil {
				return trace.Wrap(err)
			}

			a.state.SetID(evt.ID)
			a.state.SetCursor(evt.Cursor)

			if evt.IsSessionEnd {
				func(evt *TeleportEvent) {
					a.SpawnCritical(func(ctx context.Context) error {
						return a.startSessionPoll(ctx, evt)
					})
				}(evt)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// sendEvent sends an event to fluentd
func (a *App) sendEvent(ctx context.Context, url string, e *TeleportEvent) error {
	log := logger.Get(ctx)

	if !a.config.DryRun {
		err := a.fluentd.Send(ctx, url, e.Event)
		if err != nil {
			return trace.Wrap(err)
		}
	}

	fields := logrus.Fields{"id": e.ID, "type": e.Type, "ts": e.Time, "index": e.Index}
	if e.SessionID != "" {
		fields["sid"] = e.SessionID
	}

	log.WithFields(fields).Info("Event sent")
	log.WithField("event", e).Debug("Event dump")

	return nil
}

// init initializes application state
func (a *App) init(ctx context.Context) error {
	log := logger.Get(ctx)

	a.config.Dump(ctx)

	s, err := NewState(a.config)
	if err != nil {
		return trace.Wrap(err)
	}

	err = a.setStartTime(ctx, s)
	if err != nil {
		return trace.Wrap(err)
	}

	f, err := NewFluentdClient(&a.config.FluentdConfig)
	if err != nil {
		return trace.Wrap(err)
	}

	latestCursor, err := s.GetCursor()
	if err != nil {
		return trace.Wrap(err)
	}

	latestID, err := s.GetID()
	if err != nil {
		return trace.Wrap(err)
	}

	startTime, err := s.GetStartTime()
	if err != nil {
		return trace.Wrap(err)
	}

	t, err := NewTeleportEventsWatcher(ctx, a.config, *startTime, latestCursor, latestID)
	if err != nil {
		return trace.Wrap(err)
	}

	a.state = s
	a.fluentd = f
	a.teleport = t

	log.WithField("cursor", latestCursor).Info("Using initial cursor value")
	log.WithField("id", latestID).Info("Using initial ID value")
	log.WithField("value", startTime).Info("Using start time from state")

	return nil
}

// setStartTime sets start time or fails if start time has changed from the last run
func (a *App) setStartTime(ctx context.Context, s *State) error {
	log := logger.Get(ctx)

	prevStartTime, err := s.GetStartTime()
	if err != nil {
		return trace.Wrap(err)
	}

	if prevStartTime == nil {
		log.WithField("value", a.config.StartTime).Debug("Setting start time")

		t := a.config.StartTime
		if t == nil {
			now := time.Now().UTC().Truncate(time.Second)
			t = &now
		}

		return s.SetStartTime(t)
	}

	// If there is a time saved in the state and this time does not equal to the time passed from CLI and a
	// time was explicitly passed from CLI
	if prevStartTime != nil && a.config.StartTime != nil && *prevStartTime != *a.config.StartTime {
		return trace.Errorf("You can not change start time in the middle of ingestion. To restart the ingestion, rm -rf %v", a.config.StorageDir)
	}

	return nil
}

// contextWithCancelOnTerminate creates child context which is canceled when app receives onTerminate signal (graceful shutdown)
func (a *App) contextWithCancelOnTerminate(ctx context.Context) context.Context {
	process := lib.MustGetProcess(ctx)
	ctx, cancel := context.WithCancel(ctx)
	process.OnTerminate(func(_ context.Context) error {
		cancel()
		return nil
	})
	return ctx
}

// takeSemaphore obtains semaphore
func (a *App) takeSemaphore(ctx context.Context) error {
	select {
	case a.semaphore <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseSemaphore releases semaphore
func (a *App) releaseSemaphore(ctx context.Context) error {
	select {
	case <-a.semaphore:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
