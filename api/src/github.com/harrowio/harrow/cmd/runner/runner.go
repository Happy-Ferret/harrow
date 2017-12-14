package runner

import (
	"net/url"
	"strings"
	"time"

	"github.com/harrowio/harrow/bus/activity"
	"github.com/harrowio/harrow/config"
	"github.com/harrowio/harrow/domain"
	"github.com/harrowio/harrow/logger"
	"github.com/harrowio/harrow/stores"
	"github.com/harrowio/harrow/uuidhelper"
	"github.com/jmoiron/sqlx"
	"github.com/pkg/errors"
)

type state string

type Runnable interface {
	Next() (*domain.Operation, error)
}

type Runner struct {
	// The runnable of operations to run
	source Runnable

	// Storage
	activitySink activity.Sink
	db           *sqlx.DB

	// externally set fields
	config   *config.Config
	interval int
	errs     chan error

	// tracking
	log      logger.Logger
	reporter Reporter

	// internally set
	connURL *url.URL
	lxd     *LXD

	// internal management channels/etc
	operationPending chan *domain.Operation
	quitSearching    chan bool
	stopper          chan chan bool
	healthTicker     *time.Ticker

	// state (used for metrics and expvars)
	stateChange chan state
	state       state
}

func (r *Runner) Start() {

	var (
		healthChecks <-chan time.Time = make(chan time.Time)
		err          error
	)

	r.lxd = &LXD{
		config:   r.config,
		connURL:  r.connURL,
		log:      r.log,
		reporter: r.reporter,
	}

	r.db, err = r.config.DB()
	if err != nil {
		r.errs <- errors.Wrap(err, "can't dial db in runner")
	}

	r.stopper = make(chan chan bool)
	r.stateChange = make(chan state)
	r.operationPending = make(chan *domain.Operation)
	r.quitSearching = make(chan bool)

	connectionLost := make(chan error)
	containerNetworkUp := make(chan error)
	go r.MakeContainer(uuidhelper.MustNewV4(), containerNetworkUp)

	for {
		select {

		// we have a pending job, we should only recieve on this channel after we
		// successfully start a container as the Runnable fetcher only runs once we
		// get a message on the containerNetworkUp channel. The runOperation
		// goroutine will send something (maybe nil) on the err channel upon
		// completion. This gives whoever started us chance to maybe start again
		// incase we "err" out successfully
		case op := <-r.operationPending:
			go r.runOperation(op)

		// were we able to start a container?
		case res := <-containerNetworkUp:

			// if starting a container failed proporgate that
			if res != nil {
				r.errs <- res
			}

			// let the log know our container is up
			r.log.Info().Msg("initial container health check passed, entering wait mode")

			// setup a ticker for the health checks
			interval := time.Duration(r.interval) * time.Second
			r.healthTicker = time.NewTicker(interval)
			healthChecks = r.healthTicker.C
			r.log.Info().Msgf("health check interval is %s", interval)

			pgDSN, err := r.config.PgDataSourceName()
			if err != nil {
				r.errs <- errors.Wrap(err, "couldn't get postgresql dsn")
			}

			go r.lxd.MaintainConnection(connectionLost)
			go func(quit chan bool, sendOn chan<- *domain.Operation, db *sqlx.DB) {
				opdob := OperationFromDbOrBus{
					db:        db,
					dbConnStr: pgDSN,

					log:      r.log,
					reporter: r.reporter,
				}
				if err := opdob.NextOn(quit, sendOn); err != nil {
					r.log.Info().Msgf("searching goroutine sending on err chan (%s)", err)
					r.errs <- err
				}
				r.log.Info().Msgf("got one operation, cancelling out of searching goroutine")
			}(r.quitSearching, r.operationPending, r.db)

		// Incase we got a stop signal break this loop
		case stopped := <-r.stopper:
			if r.healthTicker != nil {
				r.healthTicker.Stop()
			}
			if err := r.lxd.DestroyContainer(); err != nil {
				r.log.Error().Msgf("error destroying container: %s", err)
			}
			go func() { r.quitSearching <- true }()
			stopped <- true
			r.db.Close()
			return

		// healchChecks might be a channel that never yields (if we never go into
		// <-containerNetworkUp), but then we should exit with an error immediately
		case err := <-connectionLost:
			r.reporter.LongPollLost()
			r.errs <- errors.Wrap(err, "long running container connection lost")

		// healchChecks might be a channel that never yields (if we never go into
		// <-containerNetworkUp), but then we should exit with an error immediately
		case <-healthChecks:
			if err := r.lxd.CheckContainerNetworking(); err == nil {
				r.reporter.Heartbeat()
			} else {
				r.errs <- errors.Wrap(err, "container failed periodic networking health check")
			}
		}
	}

}

func (r *Runner) Stop(reason string) {
	r.log.Info().Msgf("sending stop signal on stopper channel (%s)", reason)
	if r.stopper == nil {
		r.log.Info().Msgf("already got stop signal, please be patient")
	} else {
		stopped := make(chan bool)
		r.stopper <- stopped
		r.stopper = nil
		r.log.Info().Msg("waiting for clean shutdown...")
		<-stopped
		r.reporter.CleanShutdown()
		r.log.Info().Msg("got clean shutdown notice, returning from Stop()")
	}
}

func (r *Runner) SetLXDConnStr(connStr string) (err error) {
	r.connURL, err = url.Parse(connStr)
	if err != nil {
		return errors.Wrap(err, "error parsing connection string as URL")
	}
	return nil
}

func (r *Runner) MakeContainer(uuid string, res chan error) {

	r.lxd.containerUUID = uuid

	r.log.Debug().Msg("checking container health")
	exists, err := r.lxd.ContainerExists()
	if !exists {
		r.log.Debug().Msg("container does not exist")
	} else {
		r.log.Debug().Msg("container exists")
	}
	if err != nil {
		res <- err
	}
	if !exists {
		if err := r.lxd.MakeContainer(); err != nil {
			res <- err
		}
		res <- r.lxd.WaitForContainerNetworking(5 * time.Minute)
	}

}

func (r *Runner) runOperation(op *domain.Operation) {

	tx, err := r.db.Beginx()
	if err != nil {
		r.errs <- errors.Wrap(err, "could not start database transaction")
	}

	r.log.Info().Msgf("operation to run is: %s", op.Uuid)
	o := Operation{
		op:     op,
		db:     r.db,
		config: r.config,
		lxd:    r.lxd,
		log:    r.log,
	}

	r.log.Info().Msg("marking operation as proceeding to run it")
	stores.NewDbOperationStore(tx).MarkAsStarted(op.Uuid)
	tx.Commit()

	switch op.IsUserJob() {
	case true:
		r.log.Info().Msg("operation is a user job (will run in lxd)")
		o.RunOnLXDHost()
	case false:
		notifierType := strings.Split(*op.NotifierType, "_")[0]
		r.log.Info().Msg("operation is a notifier job (will run in the local shell)")
		o.RunLocally(notifierType)
	}

	r.errs <- nil
}
