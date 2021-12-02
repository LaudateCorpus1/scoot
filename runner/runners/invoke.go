package runners

import (
	e "errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/twitter/scoot/bazel"
	"github.com/twitter/scoot/bazel/cas"
	"github.com/twitter/scoot/bazel/execution/bazelapi"
	"github.com/twitter/scoot/common/errors"
	"github.com/twitter/scoot/common/log/tags"
	mem_os "github.com/twitter/scoot/common/os"
	mem_exec "github.com/twitter/scoot/common/os/exec"
	scootproto "github.com/twitter/scoot/common/proto"
	"github.com/twitter/scoot/common/stats"
	"github.com/twitter/scoot/runner"
	"github.com/twitter/scoot/runner/execer"
	"github.com/twitter/scoot/runner/execer/execers"
	"github.com/twitter/scoot/snapshot"
	bzsnapshot "github.com/twitter/scoot/snapshot/bazel"
)

// invoke.go: Invoker runs a Scoot command.

var memNotFreedByteAlertThreshold = 1000000 // TODO should we use a value other than 1M?
var processesForMemMonitor = []string{"bazel", "pants"}

// NewInvoker creates an Invoker that will use the supplied helpers
func NewInvoker(
	exec execer.Execer,
	filerMap runner.RunTypeMap,
	output runner.OutputCreator,
	stat stats.StatsReceiver,
	dirMonitor *stats.DirsMonitor,
	rID runner.RunnerID,
) *Invoker {
	if stat == nil {
		stat = stats.NilStatsReceiver()
	}
	mem_execer := mem_exec.NewOsExec()
	return &Invoker{exec: exec, filerMap: filerMap, output: output, stat: stat, dirMonitor: dirMonitor, rID: rID, mem_execer: mem_execer}
}

// Invoker Runs a Scoot Command by performing the Scoot setup and gathering.
// (E.g., checking out a Snapshot, or saving the Output once it's done)
// Unlike a full Runner, it has no idea of what else is running or has run.
type Invoker struct {
	exec       execer.Execer // execer for running tasks as killable commands
	filerMap   runner.RunTypeMap
	output     runner.OutputCreator
	stat       stats.StatsReceiver
	dirMonitor *stats.DirsMonitor
	rID        runner.RunnerID
	mem_execer mem_exec.OsExec
}

// Run runs cmd
// Run will send updates as the process is running to updateCh.
// The RunStatus'es that come out of updateCh will have an empty RunID
// Run will enforce cmd's Timeout, and will abort cmd if abortCh is signaled.
// updateCh will not close until the run is finished running.
func (inv *Invoker) Run(cmd *runner.Command, id runner.RunID) (abortCh chan<- struct{}, updateCh <-chan runner.RunStatus) {
	abortChFull := make(chan struct{})
	memChFull := make(chan execer.ProcessStatus)
	updateChFull := make(chan runner.RunStatus)
	go inv.run(cmd, id, abortChFull, memChFull, updateChFull)
	return abortChFull, updateChFull
}

// Run runs cmd as run id returning the final ProcessStatus
// Run will send updates the process is running to updateCh.
// Run will enforce cmd's Timeout, and will abort cmd if abortCh is signaled.
// Run will not return until the process is not running.
func (inv *Invoker) run(cmd *runner.Command, id runner.RunID, abortCh chan struct{}, memCh chan execer.ProcessStatus, updateCh chan runner.RunStatus) (r runner.RunStatus) {
	log.WithFields(
		log.Fields{
			"runID":  id,
			"tag":    cmd.Tag,
			"jobID":  cmd.JobID,
			"taskID": cmd.TaskID,
		}).Info("*Invoker.run()")
	inv.stat.Gauge(stats.WorkerRunningTask).Update(1)
	defer inv.stat.Gauge(stats.WorkerRunningTask).Update(0)

	taskTimer := inv.stat.Latency(stats.WorkerTaskLatency_ms).Time()

	defer func() {
		taskTimer.Stop()
		updateCh <- r
		close(updateCh)
	}()

	start := time.Now()

	// Records various stages of the run
	// TODO opporunity for consolidation with existing timers and metrics as part of larger refactor
	rts := &runTimes{}
	rts.invokeStart = stamp()

	var co snapshot.Checkout
	checkoutCh := make(chan error)

	startMem := inv.getWorkerCurrentMem()

	// Determine RunType from Command SnapshotID
	// This invoker supports RunTypeScoot and RunTypeBazel
	var runType runner.RunType
	if err := bazel.ValidateID(cmd.SnapshotID); err == nil {
		runType = runner.RunTypeBazel
	} else {
		runType = runner.RunTypeScoot
	}
	if _, ok := inv.filerMap[runType]; !ok {
		return runner.FailedStatus(id,
			errors.NewError(fmt.Errorf("Invoker does not have filer for command of RunType: %s", runType), errors.PreProcessingFailureExitCode),
			tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})
	}

	// monitor memory not released by the processing of the current task
	defer inv.monitorTaskMemoryAccum(cmd, id, runType, startMem)

	// Bazel requests - fetch command argv/env from CAS
	// We can also receive a cached result here, in which case we skip invocation
	rts.inputStart = stamp()
	if runType == runner.RunTypeBazel {
		cachedResult, notExist, err := preProcessBazel(inv.filerMap[runType].Filer, cmd, rts)
		if err != nil {
			msg := fmt.Sprintf("Error preprocessing Bazel command: %s", err)
			failedStatus := runner.FailedStatus(id, errors.NewError(
				e.New(msg), errors.PreProcessingFailureExitCode),
				tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})
			failedStatus.ActionResult = &bazelapi.ActionResult{GRPCStatus: getInternalErrorStatus(msg)}

			// If we encounter a cas NotFoundError, set a grpc status message in
			// the failure run status that indicates missing data to client
			if cas.IsNotFoundError(err) {
				log.Info("NotFound error during Bazel preprocess - Setting grpc Status error")
				errStatus, err := getFailedPreconditionStatus(notExist)
				if err != nil {
					log.Errorf("Error generating Failed Precondition status: %s", err)
				} else {
					failedStatus.ActionResult = &bazelapi.ActionResult{GRPCStatus: errStatus}
				}
			}
			return failedStatus
		}
		if cachedResult != nil {
			rts.queuedTime = scootproto.GetTimeFromTimestamp(cmd.ExecuteRequest.GetExecutionMetadata().GetQueuedTimestamp())
			queuedDuration := rts.invokeStart.Sub(rts.queuedTime)
			actionCacheCheckTime := rts.actionCacheCheckEnd.Sub(rts.actionCacheCheckStart)
			inv.stat.Histogram(stats.BzExecQueuedTimeHistogram_ms).Update(int64(queuedDuration / time.Millisecond))
			inv.stat.Histogram(stats.BzExecActionCacheCheckTimeHistogram_ms).Update(int64(actionCacheCheckTime / time.Millisecond))
			inv.stat.Counter(stats.BzCachedExecCounter).Inc(1)
			status := runner.CompleteStatus(id, "", 0,
				tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})
			status.ActionResult = cachedResult
			return status
		}
	}

	// if we are checking out a snapshot, start the timer outside of go routine
	var downloadTimer stats.Latency
	if cmd.SnapshotID != "" {
		downloadTimer = inv.stat.Latency(stats.WorkerDownloadLatency_ms).Time()
		inv.stat.Counter(stats.WorkerDownloads).Inc(1)
	}

	// update local workspace with snapshot
	go func() {
		if cmd.SnapshotID == "" {
			// TODO: we don't want this logic to live here, these decisions should be made at a higher level.
			if len(cmd.Argv) > 0 && cmd.Argv[0] != execers.UseSimExecerArg {
				log.WithFields(
					log.Fields{
						"runID":  id,
						"tag":    cmd.Tag,
						"jobID":  cmd.JobID,
						"taskID": cmd.TaskID,
					}).Info("No snapshotID! Using a nop-checkout initialized with tmpDir")
			}
			if tmp, err := ioutil.TempDir("", "invoke_nop_checkout"); err != nil {
				checkoutCh <- err
			} else {
				co = snapshot.NewNopCheckout(string(id), tmp)
				checkoutCh <- nil
			}
		} else {
			log.WithFields(
				log.Fields{
					"runID":      id,
					"tag":        cmd.Tag,
					"jobID":      cmd.JobID,
					"taskID":     cmd.TaskID,
					"snapshotID": cmd.SnapshotID,
				}).Info("Checking out snapshotID")
			var err error
			co, err = inv.filerMap[runType].Filer.Checkout(cmd.SnapshotID)
			checkoutCh <- err
		}
	}()

	// wait for checkout to finish (or abort signal)
	select {
	case <-abortCh:
		if err := inv.filerMap[runType].Filer.CancelCheckout(); err != nil {
			log.Errorf("Error canceling checkout: %s", err)
		}
		if err := <-checkoutCh; err != nil {
			log.Errorf("Checkout errored: %s", err)
			// If there was an error there should be no lingering gitdb locks, so return
			// In addition, co should be nil, so failing to return and calling co.Release()
			// will result in a nil pointer dereference
		} else {
			// If there was no error then we need to release this checkout.
			co.Release()
		}
		return runner.AbortStatus(id,
			tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})
	case err := <-checkoutCh:
		// stop the timer
		// note: aborted runs don't stop the timer - the reported download time should remain 0
		// successful and erroring downloads will report time values
		if cmd.SnapshotID != "" {
			downloadTimer.Stop()
		}
		if err != nil {
			var failedStatus runner.RunStatus
			codeErr, ok := err.(*errors.ExitCodeError)
			switch ok {
			case true:
				// err is of type github.com/twitter/scoot/common/errors.Error
				failedStatus = runner.FailedStatus(id, codeErr,
					tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})
			default:
				// err is not of type github.com/twitter/scoot/common/errors.Error
				failedStatus = runner.FailedStatus(id, errors.NewError(err, errors.GenericCheckoutFailureExitCode),
					tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})
			}

			// For Checkout errors from Bazel commands that indicate non-existence, we set a GRPC
			// Status error indicating that the InputRoot data could not be found.
			if runType == runner.RunTypeBazel {
				msg := fmt.Sprintf("Failed to checkout Snapshot: %s", err)
				failedStatus.ActionResult = &bazelapi.ActionResult{GRPCStatus: getInternalErrorStatus(msg)}
				if _, ok := err.(*bzsnapshot.CheckoutNotExistError); ok {
					log.Info("Checkout for Bazel command returned CheckoutNotExistError - Setting grpc Status error")
					errStatus, err := getCheckoutMissingStatus(cmd.SnapshotID)
					if err != nil {
						log.Errorf("Error generating Failed Precondition status: %s", err)
					} else {
						failedStatus.ActionResult = &bazelapi.ActionResult{GRPCStatus: errStatus}
					}
				}
			}
			return failedStatus
		}
		// Checkout is ok, continue with run and when finished release checkout.
		defer co.Release()
		rts.inputEnd = stamp()
	}
	log.WithFields(
		log.Fields{
			"runID":    id,
			"tag":      cmd.Tag,
			"jobID":    cmd.JobID,
			"taskID":   cmd.TaskID,
			"checkout": co.Path(),
		}).Info("Checkout done")

	// setup stdout,stderr output
	stdout, err := inv.output.Create(fmt.Sprintf("%s-stdout", id))
	if err != nil {
		msg := fmt.Sprintf("could not create stdout: %s", err)
		failedStatus := runner.FailedStatus(id, errors.NewError(e.New(msg), errors.LogRefCreationFailureExitCode),
			tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})
		if runType == runner.RunTypeBazel {
			failedStatus.ActionResult = &bazelapi.ActionResult{GRPCStatus: getInternalErrorStatus(msg)}
		}
		return failedStatus
	}
	defer stdout.Close()

	stderr, err := inv.output.Create(fmt.Sprintf("%s-stderr", id))
	if err != nil {
		msg := fmt.Sprintf("could not create stderr: %s", err)
		failedStatus := runner.FailedStatus(id, errors.NewError(e.New(msg), errors.LogRefCreationFailureExitCode),
			tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})
		if runType == runner.RunTypeBazel {
			failedStatus.ActionResult = &bazelapi.ActionResult{GRPCStatus: getInternalErrorStatus(msg)}
		}
		return failedStatus
	}
	defer stderr.Close()

	stdlog, err := inv.output.Create(fmt.Sprintf("%s-stdlog", id))
	if err != nil {
		msg := fmt.Sprintf("could not create combined stdout/stderr: %s", err)
		failedStatus := runner.FailedStatus(id, errors.NewError(e.New(msg), errors.LogRefCreationFailureExitCode),
			tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})
		if runType == runner.RunTypeBazel {
			failedStatus.ActionResult = &bazelapi.ActionResult{GRPCStatus: getInternalErrorStatus(msg)}
		}
		return failedStatus
	}
	defer stdlog.Close()

	marker := "###########################################\n###########################################\n"
	format := "%s\n\nDate: %v\nOut: %s\tErr: %s\tOutErr: %s\tCmd:\n%v\n\n%s\n\n\nSCOOT_CMD_LOG\n"
	header := fmt.Sprintf(format, marker, time.Now(), stdout.URI(), stderr.URI(), stdlog.URI(), cmd, marker)
	// NOTE We don't add headers for Bazel.
	// If we wanted to allow optionally, a switch for this would come either at the Worker level
	// (via Invoker -> QueueRunner construction), or the Command level (job requestor specifies in e.g. a PlatformProperty)

	// Processing/setup post checkout before execution
	switch runType {
	case runner.RunTypeBazel:
		for _, pp := range cmd.ExecuteRequest.GetCommand().GetPlatform().GetProperties() {
			if pp.GetName() == "JDK_SYMLINK" {
				log.Infof("JDK_SYMLINK platform property identified. Creating %s symlink", pp.GetValue())
				parentDir, _ := filepath.Split(co.Path() + "/")
				err = setupJDKSymlink(parentDir, pp.GetValue())
				if err != nil {
					msg := fmt.Sprintf("Failed setting up JDK symlink to %s: %s", pp.GetValue(), err)
					failedStatus := runner.FailedStatus(id, errors.NewError(e.New(msg), errors.PostProcessingFailureExitCode),
						tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})
					failedStatus.ActionResult = &bazelapi.ActionResult{GRPCStatus: getInternalErrorStatus(msg)}
					return failedStatus
				}
			}
		}

		err = createOutputPaths(cmd, co.Path())
		if err != nil {
			msg := fmt.Sprintf("Failed setting up output directories: %s", err)
			failedStatus := runner.FailedStatus(id, errors.NewError(e.New(msg), errors.PostProcessingFailureExitCode),
				tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})
			failedStatus.ActionResult = &bazelapi.ActionResult{GRPCStatus: getInternalErrorStatus(msg)}
			return failedStatus
		}
	case runner.RunTypeScoot:
		stdout.Write([]byte(header))
		stderr.Write([]byte(header))
		stdlog.Write([]byte(header))
	}

	inv.dirMonitor.GetStartSizes() // start monitoring directory sizes

	// start running the command
	log.WithFields(
		log.Fields{
			"runID":  id,
			"tag":    cmd.Tag,
			"jobID":  cmd.JobID,
			"taskID": cmd.TaskID,
			"stdout": stdout.AsFile(),
			"stderr": stderr.AsFile(),
			"stdlog": stdlog.AsFile(),
		}).Debug("Stdout/Stderr output")
	rts.execStart = stamp() // candidate for availability via Execer
	p, err := inv.exec.Exec(execer.Command{
		Argv:    cmd.Argv,
		EnvVars: cmd.EnvVars,
		Dir:     co.Path(),
		Stdout:  io.MultiWriter(stdout, stdlog),
		Stderr:  io.MultiWriter(stderr, stdlog),
		MemCh:   memCh,
		LogTags: cmd.LogTags,
	})
	if err != nil {
		msg := fmt.Sprintf("could not exec: %s", err)
		failedStatus := runner.FailedStatus(id, errors.NewError(e.New(msg), errors.CouldNotExecExitCode),
			tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})
		if runType == runner.RunTypeBazel {
			failedStatus.ActionResult = &bazelapi.ActionResult{GRPCStatus: getInternalErrorStatus(msg)}
		}
		return failedStatus
	}

	var timeoutCh <-chan time.Time
	if cmd.Timeout > 0 { // Timeout if applicable
		elapsed := time.Now().Sub(start)
		timeout := time.NewTimer(cmd.Timeout - elapsed)
		timeoutCh = timeout.C
		defer timeout.Stop()
	}

	updateCh <- runner.RunningStatus(id, stdout.URI(), stderr.URI(),
		tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})

	processCh := make(chan execer.ProcessStatus, 1)
	go func() { processCh <- p.Wait() }()
	var st execer.ProcessStatus

	// Wait for process to complete (or cancel if we're told to)
	select {
	case <-abortCh:
		stdout.Write([]byte(fmt.Sprintf("\n\n%s\n\nFAILED\n\nTask aborted: %v", marker, cmd.String())))
		stderr.Write([]byte(fmt.Sprintf("\n\n%s\n\nFAILED\n\nTask aborted: %v", marker, cmd.String())))
		stdlog.Write([]byte(fmt.Sprintf("\n\n%s\n\nFAILED\n\nTask aborted: %v", marker, cmd.String())))
		p.Abort()
		return runner.AbortStatus(id,
			tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})
	case <-timeoutCh:
		stdout.Write([]byte(fmt.Sprintf("\n\n%s\n\nFAILED\n\nTask exceeded timeout %v: %v", marker, cmd.Timeout, cmd.String())))
		stderr.Write([]byte(fmt.Sprintf("\n\n%s\n\nFAILED\n\nTask exceeded timeout %v: %v", marker, cmd.Timeout, cmd.String())))
		stdlog.Write([]byte(fmt.Sprintf("\n\n%s\n\nFAILED\n\nTask exceeded timeout %v: %v", marker, cmd.Timeout, cmd.String())))
		p.Abort()
		log.WithFields(
			log.Fields{
				"cmd":    cmd.String(),
				"tag":    cmd.Tag,
				"jobID":  cmd.JobID,
				"taskID": cmd.TaskID,
			}).Info("Run timedout")
		return runner.TimeoutStatus(id,
			tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})
	case st = <-memCh:
		stdout.Write([]byte(fmt.Sprintf("\n\n%s\n\nFAILED\n\n%v", marker, st.Error)))
		stderr.Write([]byte(fmt.Sprintf("\n\n%s\n\nFAILED\n\n%v", marker, st.Error)))
		stdlog.Write([]byte(fmt.Sprintf("\n\n%s\n\nFAILED\n\n%v", marker, st.Error)))
		log.WithFields(
			log.Fields{
				"runID":    id,
				"cmd":      cmd.String(),
				"tag":      cmd.Tag,
				"jobID":    cmd.JobID,
				"taskID":   cmd.TaskID,
				"status":   st,
				"checkout": co.Path(),
			}).Infof("Cmd exceeded MemoryCap, aborting %v", cmd.String())
	case st = <-processCh:
		// Process has completed
		log.WithFields(
			log.Fields{
				"runID":    id,
				"cmd":      cmd.String(),
				"tag":      cmd.Tag,
				"jobID":    cmd.JobID,
				"taskID":   cmd.TaskID,
				"status":   st,
				"checkout": co.Path(),
			}).Info("Run done")
	}

	// record command's disk usage for the monitored directories
	inv.dirMonitor.GetEndSizes()
	inv.dirMonitor.RecordSizeStats(inv.stat)

	// the command is no longer running, post process the results
	switch st.State {
	case execer.COMPLETE:
		rts.execEnd = stamp()
		rts.outputStart = stamp()
		if runType == runner.RunTypeScoot {
			tmp, err := ioutil.TempDir("", "invoke")
			if err != nil {
				return runner.FailedStatus(id, errors.NewError(fmt.Errorf("error staging ingestion dir: %v", err), errors.PostExecFailureExitCode),
					tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})
			}
			uploadTimer := inv.stat.Latency(stats.WorkerUploadLatency_ms).Time()
			inv.stat.Counter(stats.WorkerUploads).Inc(1)
			defer func() {
				os.RemoveAll(tmp)
				uploadTimer.Stop()
			}()

			stdoutName := "STDOUT"
			stderrName := "STDERR"
			stdlogName := "STDLOG"
			if err = stageLogFiles(tmp, stdoutName, stderrName, stdlogName, stdout, stderr, stdlog); err != nil {
				return runner.FailedStatus(id, errors.NewError(err, errors.PostExecFailureExitCode),
					tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})
			}

			ingestCh := make(chan interface{})
			go func() {
				snapshotID, err := inv.filerMap[runType].Filer.Ingest(tmp)
				if err != nil {
					ingestCh <- err
				} else {
					ingestCh <- snapshotID
				}
			}()

			var snapshotID string
			select {
			case <-abortCh:
				if err := inv.filerMap[runType].Filer.CancelIngest(); err != nil {
					log.Errorf("Error canceling ingest: %s", err)
				}
				// Cancel call above should cause ingest to exit sooner, but still wait for return to release worker
				<-ingestCh
				return runner.AbortStatus(id, tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})
			case res := <-ingestCh:
				switch res.(type) {
				case error:
					log.WithFields(
						log.Fields{
							"tag":    cmd.Tag,
							"jobID":  cmd.JobID,
							"taskID": cmd.TaskID,
						}).Errorf("Error ingesting results: %v", res)
				default:
					snapshotID = res.(string)
				}
			}
			rts.outputEnd = stamp()
			rts.invokeEnd = stamp()

			// Note: only modifying stdout/stderr refs when we're actively working with snapshotID.
			status := runner.CompleteStatus(id, snapshotID, st.ExitCode,
				tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})
			if cmd.SnapshotID != "" {
				status.StdoutRef = snapshotID + "/" + stdoutName
				status.StderrRef = snapshotID + "/" + stderrName
			}
			if st.Error != "" {
				status.Error = st.Error
			}
			return status
		} else if runType == runner.RunTypeBazel {
			uploadTimer := inv.stat.Latency(stats.WorkerUploadLatency_ms).Time()
			inv.stat.Counter(stats.WorkerUploads).Inc(1)
			defer func() {
				uploadTimer.Stop()
			}()

			// Process Bazel uploads of std* output and other data to CAS
			ingestCh := make(chan interface{})
			go func() {
				actionResult, err := postProcessBazel(inv.filerMap[runType].Filer, cmd, co.Path(), stdout, stderr, st, rts, inv.rID)
				if err != nil {
					ingestCh <- err
				} else {
					ingestCh <- actionResult
				}
			}()

			var actionResult *bazelapi.ActionResult
			select {
			case <-abortCh:
				if err := inv.filerMap[runType].Filer.CancelIngest(); err != nil {
					log.Errorf("Error canceling ingest: %s", err)
				}
				// Cancel call above should cause ingest to exit sooner, but still wait for return to release worker
				<-ingestCh
				return runner.AbortStatus(id, tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})
			case res := <-ingestCh:
				switch res.(type) {
				case error:
					msg := fmt.Sprintf("Error postprocessing Bazel command: %s", res)
					failedStatus := runner.FailedStatus(id, errors.NewError(e.New(msg), errors.PostExecFailureExitCode),
						tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})
					failedStatus.ActionResult = &bazelapi.ActionResult{GRPCStatus: getInternalErrorStatus(msg)}
					return failedStatus
				}
				actionResult = res.(*bazelapi.ActionResult)
			}

			queuedDuration := rts.invokeStart.Sub(rts.queuedTime)
			actionCacheCheckTime := rts.actionCacheCheckEnd.Sub(rts.actionCacheCheckStart)
			actionFetchTime := rts.actionFetchEnd.Sub(rts.actionFetchStart)
			commandFetchTime := rts.commandFetchEnd.Sub(rts.commandFetchStart)
			inputTime := rts.inputEnd.Sub(rts.inputStart)
			execerTime := rts.execEnd.Sub(rts.execStart)
			inv.stat.Histogram(stats.BzExecQueuedTimeHistogram_ms).Update(int64(queuedDuration / time.Millisecond))
			inv.stat.Histogram(stats.BzExecActionCacheCheckTimeHistogram_ms).Update(int64(actionCacheCheckTime / time.Millisecond))
			inv.stat.Histogram(stats.BzExecActionFetchTimeHistogram_ms).Update(int64(actionFetchTime / time.Millisecond))
			inv.stat.Histogram(stats.BzExecCommandFetchTimeHistogram_ms).Update(int64(commandFetchTime / time.Millisecond))
			inv.stat.Histogram(stats.BzExecInputFetchTimeHistogram_ms).Update(int64(inputTime / time.Millisecond))
			inv.stat.Histogram(stats.BzExecExecerTimeHistogram_ms).Update(int64(execerTime / time.Millisecond))

			status := runner.CompleteStatus(id, "", st.ExitCode,
				tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})
			status.ActionResult = actionResult
			if st.Error != "" {
				status.Error = st.Error
			}
			return status
		} else {
			// should never have an unknown RunType here
			return runner.FailedStatus(id, errors.NewError(fmt.Errorf("Can't process Completed status for RunType %s", runType), errors.PostExecFailureExitCode),
				tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})
		}
	case execer.FAILED:
		msg := fmt.Sprintf("error execing: %s", st.Error)
		failedStatus := runner.FailedStatus(id, errors.NewError(e.New(msg), errors.PostExecFailureExitCode),
			tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})
		if runType == runner.RunTypeBazel {
			failedStatus.ActionResult = &bazelapi.ActionResult{GRPCStatus: getInternalErrorStatus(msg)}
		}
		return failedStatus
	default:
		msg := "unexpected exec state"
		failedStatus := runner.FailedStatus(id, errors.NewError(e.New(msg), errors.PostExecFailureExitCode),
			tags.LogTags{JobID: cmd.JobID, TaskID: cmd.TaskID, Tag: cmd.Tag})
		if runType == runner.RunTypeBazel {
			failedStatus.ActionResult = &bazelapi.ActionResult{GRPCStatus: getInternalErrorStatus(msg)}
		}
		return failedStatus
	}
}

// getWorkerCurrentMem get the sum of the memory consumption for all processes owned by the current user
func (inv *Invoker) getWorkerCurrentMem() int {
	memGetter := mem_os.NewMemory(inv.mem_execer)
	mem, err := memGetter.GetUserCurrentMem()
	if err != nil {
		log.Errorf("couldn't get user memory consumption:%s", err)
		return 0
	}
	return mem
}

// monitorTaskMemoryAccum - record amount of memory allocated for this task, but not released.  This assumes all of the
// worker's memory consumption can be found via processes owned by the current user
func (inv *Invoker) monitorTaskMemoryAccum(cmd *runner.Command, id runner.RunID, runType runner.RunType, startMem int) {
	endMem := inv.getWorkerCurrentMem()
	memDelta := endMem - startMem
	// record memory not released (minimum value recorded is 0)
	if runType == runner.RunTypeBazel {
		inv.stat.Gauge(stats.WorkerBazelMemByteAccumGauge).Update(int64(math.Max(float64(memDelta), 0.0)))
		inv.stat.Gauge(stats.WorkerPantsMemByteAccumGauge).Update(int64(0))
	} else {
		inv.stat.Gauge(stats.WorkerPantsMemByteAccumGauge).Update(int64(math.Max(float64(memDelta), 0.0)))
		inv.stat.Gauge(stats.WorkerBazelMemByteAccumGauge).Update(int64(0))
	}
	// if the memory that was not released by the current task is over threshold,
	// log the task's info and amount of memory it is still holding.  The objective is to provide information
	// in the logs to recognize scenarios (task, snapshot) that are consuming and not releasing memory
	if memDelta > memNotFreedByteAlertThreshold {
		log.WithFields(
			log.Fields{
				"runID":      id,
				"cmd":        cmd.String(),
				"tag":        cmd.Tag,
				"jobID":      cmd.JobID,
				"taskID":     cmd.TaskID,
				"snapshotID": cmd.SnapshotID,
			}).Errorf("%d (bytes) memory was not released at end of command", memDelta)
	}
}

func (inv *Invoker) setMemExecer(execer mem_exec.OsExec) {
	inv.mem_execer = execer
}

// stage output files to single directory for snapshot ingestion
func stageLogFiles(tmpDir, stdoutName, stderrName, stdlogName string, stdout, stderr, stdlog runner.Output) error {
	outPath := stdout.AsFile()
	errPath := stderr.AsFile()
	logPath := stdlog.AsFile()

	if err := copyLogFile(tmpDir, stdoutName, outPath); err != nil {
		return err
	}
	if err := copyLogFile(tmpDir, stderrName, errPath); err != nil {
		return err
	}
	if err := copyLogFile(tmpDir, stdlogName, logPath); err != nil {
		return err
	}
	return nil
}

func copyLogFile(tmpDir, logName, logPath string) error {
	var writer *os.File
	var reader *os.File
	defer writer.Close()
	defer reader.Close()

	if writer, err := os.Create(filepath.Join(tmpDir, logName)); err != nil {
		return fmt.Errorf("error staging ingestion for %s: %v", logName, err)
	} else if reader, err = os.Open(logPath); err != nil {
		return fmt.Errorf("error staging ingestion for %s: %v", logPath, err)
	} else if _, err := io.Copy(writer, reader); err != nil {
		return fmt.Errorf("error staging ingestion for stdout: %v", err)
	}
	return nil
}

// Tracking timestamps for stages of an invoker run.
// Values are only set with non-zero Time when stage has completed successfully.
type runTimes struct {
	invokeStart           time.Time
	invokeEnd             time.Time
	inputStart            time.Time
	actionCacheCheckStart time.Time
	actionCacheCheckEnd   time.Time
	actionFetchStart      time.Time
	actionFetchEnd        time.Time
	commandFetchStart     time.Time
	commandFetchEnd       time.Time
	inputEnd              time.Time
	execStart             time.Time
	execEnd               time.Time
	outputStart           time.Time
	outputEnd             time.Time
	queuedTime            time.Time // set by scheduler and must be populated e.g. by task metadata
}

// Wrapper around time values to encourage "stamp()" usage so it's harder to lose track of runTimes fields.
// Longer term, we should refactor the Invoker so the checkout/exec/upload phases are
// separated from the implementation logic, which will allow these to be recorded clearly
func stamp() time.Time {
	return time.Now()
}
