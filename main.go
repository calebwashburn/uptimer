package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/benbjohnson/clock"
	uuid "github.com/satori/go.uuid"

	"github.com/cloudfoundry/uptimer/appLogValidator"
	"github.com/cloudfoundry/uptimer/cfCmdGenerator"
	"github.com/cloudfoundry/uptimer/cfWorkflow"
	"github.com/cloudfoundry/uptimer/cmdRunner"
	"github.com/cloudfoundry/uptimer/cmdStartWaiter"
	"github.com/cloudfoundry/uptimer/config"
	"github.com/cloudfoundry/uptimer/measurement"
	"github.com/cloudfoundry/uptimer/orchestrator"
	"github.com/cloudfoundry/uptimer/version"
)

func main() {

	logger := log.New(os.Stdout, "\n[UPTIMER] ", log.Ldate|log.Ltime|log.LUTC)

	configPath := flag.String("configFile", "", "Path to the config file")
	showVersion := flag.Bool("v", false, "Prints the version of uptimer and exits")
	flag.Parse()

	if *showVersion {
		fmt.Printf("version: %s\n", version.Version)
		os.Exit(0)
	}

	if *configPath == "" {
		logger.Println("Failed to load config: ", fmt.Errorf("'-configFile' flag required"))
		os.Exit(1)
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		logger.Println("Failed to load config: ", err)
		os.Exit(1)
	}

	performMeasurements := true

	logger.Println("Building included app...")
	appPath, err := compileIncludedApp("app")
	if err != nil {
		logger.Println("Failed to build included app: ", err)
		performMeasurements = false
	}
	logger.Println("Finished building included app")

	logger.Println("Building included syslog sink app...")
	sinkAppPath, err := compileIncludedApp("syslogSink")
	if err != nil {
		logger.Println("Failed to build included syslog sink app: ", err)
	}
	logger.Println("Finished building included syslog sink app")

	orcTmpDir, recentLogsTmpDir, streamingLogsTmpDir, pushTmpDir, sinkTmpDir, err := createTmpDirs()
	if err != nil {
		logger.Println("Failed to create temp dir:", err)
		performMeasurements = false
	}

	bufferedRunner, runnerOutBuf, runnerErrBuf := createBufferedRunner()

	pushCmdGenerator := cfCmdGenerator.New(pushTmpDir)
	pushWorkflow, pushOrg, _ := createWorkflow(cfg.CF, appPath, "./app")
	logger.Printf("Setting up push workflow with org %s ...", pushOrg)
	if err := bufferedRunner.RunInSequence(pushWorkflow.Setup(pushCmdGenerator)...); err != nil {
		logBufferedRunnerFailure(logger, "push workflow setup", err, runnerOutBuf, runnerErrBuf)
		performMeasurements = false
	} else {
		logger.Println("Finished setting up push workflow")
	}

	sinkCmdGenerator := cfCmdGenerator.New(sinkTmpDir)
	sinkWorkflow, sinkOrg, _ := createWorkflow(cfg.CF, sinkAppPath, "./syslogSink")
	logger.Printf("Setting up sink workflow with org %s ...", sinkOrg)
	err = bufferedRunner.RunInSequence(
		append(append(
			sinkWorkflow.Setup(sinkCmdGenerator),
			sinkWorkflow.Push(sinkCmdGenerator)...),
			sinkWorkflow.MapRoute(sinkCmdGenerator)...)...)
	if err != nil {
		logBufferedRunnerFailure(logger, "sink workflow setup", err, runnerOutBuf, runnerErrBuf)
		performMeasurements = false
	} else {
		logger.Println("Finished setting up sink workflow")
	}

	orcCmdGenerator := cfCmdGenerator.New(orcTmpDir)
	orcWorkflow, orcOrg, _ := createWorkflow(cfg.CF, appPath, "./app")

	// map a route to the sink (this needs to happen in the space with the sink app)

	// These need to happen in the space with the main app:
	// create a user-provided service with the sink route
	// bind the user-provided service to the main app?
	// restage the app

	// ...
	// add a "recent logs" measurement targeted at the sink with a period of 30 seconds

	measurements := createMeasurements(
		logger,
		orcWorkflow,
		pushWorkflow,
		cfCmdGenerator.New(recentLogsTmpDir),
		cfCmdGenerator.New(streamingLogsTmpDir),
		pushCmdGenerator,
		cfg.AllowedFailures,
	)

	orc := orchestrator.New(cfg.While, logger, orcWorkflow, cmdRunner.New(os.Stdout, os.Stderr, io.Copy), measurements)

	logger.Printf("Setting up main workflow with org %s ...", orcOrg)
	if err := orc.Setup(bufferedRunner, orcCmdGenerator); err != nil {
		logBufferedRunnerFailure(logger, "main workflow setup", err, runnerOutBuf, runnerErrBuf)
		performMeasurements = false
	} else {
		logger.Println("Finished setting up main workflow")
	}

	exitCode, err := orc.Run(performMeasurements)
	if err != nil {
		logger.Println("Failed run:", err)
	}

	logger.Println("Tearing down...")
	tearDown(
		orc,
		orcCmdGenerator,
		logger,
		pushWorkflow,
		pushCmdGenerator,
		bufferedRunner,
		runnerOutBuf,
		runnerErrBuf,
	)
	logger.Println("Finished tearing down")

	os.Exit(exitCode)
}

func loadConfig(configPath string) (*config.Config, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}

	return cfg, nil
}

func createTmpDirs() (string, string, string, string, string, error) {
	orcTmpDir, err := ioutil.TempDir("", "uptimer")
	if err != nil {
		return "", "", "", "", "", err
	}
	recentLogsTmpDir, err := ioutil.TempDir("", "uptimer")
	if err != nil {
		return "", "", "", "", "", err
	}
	streamingLogsTmpDir, err := ioutil.TempDir("", "uptimer")
	if err != nil {
		return "", "", "", "", "", err
	}
	pushTmpDir, err := ioutil.TempDir("", "uptimer")
	if err != nil {
		return "", "", "", "", "", err
	}
	sinkTmpDir, err := ioutil.TempDir("", "uptimer")
	if err != nil {
		return "", "", "", "", "", err
	}

	return orcTmpDir, recentLogsTmpDir, streamingLogsTmpDir, pushTmpDir, sinkTmpDir, nil
}

func compileIncludedApp(appName string) (string, error) {
	appPath := path.Join(
		os.Getenv("GOPATH"),
		fmt.Sprintf("/src/github.com/cloudfoundry/uptimer/%s", appName),
	)

	buildCmd := exec.Command("go", "build")
	buildCmd.Dir = appPath
	buildCmd.Env = []string{
		"GOOS=linux",
		"GOARCH=amd64",
		fmt.Sprintf("GOPATH=%s", os.Getenv("GOPATH")),
	}
	err := buildCmd.Run()

	return appPath, err
}

func createWorkflow(cfc *config.Cf, appPath, appCommand string) (cfWorkflow.CfWorkflow, string, string) {
	org := fmt.Sprintf("uptimer-org-%s", uuid.NewV4().String())
	app := fmt.Sprintf("uptimer-app-%s", uuid.NewV4().String())

	return cfWorkflow.New(
			cfc,
			org,
			fmt.Sprintf("uptimer-space-%s", uuid.NewV4().String()),
			fmt.Sprintf("uptimer-quota-%s", uuid.NewV4().String()),
			app,
			appPath,
			appCommand,
		),
		org,
		app
}

func createMeasurements(
	logger *log.Logger,
	orcWorkflow, pushWorkflow cfWorkflow.CfWorkflow,
	recentLogsCmdGenerator, streamingLogsCmdGenerator, pushCmdGenerator cfCmdGenerator.CfCmdGenerator,
	allowedFailures config.AllowedFailures,
) []measurement.Measurement {
	recentLogsBufferRunner, recentLogsRunnerOutBuf, recentLogsRunnerErrBuf := createBufferedRunner()
	recentLogsMeasurement := measurement.NewRecentLogs(
		func() []cmdStartWaiter.CmdStartWaiter {
			return orcWorkflow.RecentLogs(recentLogsCmdGenerator)
		},
		recentLogsBufferRunner,
		recentLogsRunnerOutBuf,
		recentLogsRunnerErrBuf,
		appLogValidator.New(),
	)

	streamingLogsBufferRunner, streamingLogsRunnerOutBuf, streamingLogsRunnerErrBuf := createBufferedRunner()
	streamingLogsMeasurement := measurement.NewStreamingLogs(
		func() (context.Context, context.CancelFunc, []cmdStartWaiter.CmdStartWaiter) {
			ctx, cancelFunc := context.WithTimeout(context.Background(), 15*time.Second)
			return ctx, cancelFunc, orcWorkflow.StreamLogs(ctx, streamingLogsCmdGenerator)
		},
		streamingLogsBufferRunner,
		streamingLogsRunnerOutBuf,
		streamingLogsRunnerErrBuf,
		appLogValidator.New(),
	)

	pushRunner, pushRunnerOutBuf, pushRunnerErrBuf := createBufferedRunner()
	appPushabilityMeasurement := measurement.NewAppPushability(
		func() []cmdStartWaiter.CmdStartWaiter {
			return append(pushWorkflow.Push(pushCmdGenerator), pushWorkflow.Delete(pushCmdGenerator)...)
		},
		pushRunner,
		pushRunnerOutBuf,
		pushRunnerErrBuf,
	)

	httpAvailabilityMeasurement := measurement.NewHTTPAvailability(
		orcWorkflow.AppUrl(),
		&http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	)

	authFailedRetryFunc := func(stdOut, stdErr string) bool {
		authFailedMessage := "Authentication has expired.  Please log back in to re-authenticate."
		return strings.Contains(stdOut, authFailedMessage) || strings.Contains(stdErr, authFailedMessage)
	}

	clock := clock.New()
	return []measurement.Measurement{
		measurement.NewPeriodic(
			logger,
			clock,
			time.Second,
			httpAvailabilityMeasurement,
			measurement.NewResultSet(),
			allowedFailures.HttpAvailability,
			func(string, string) bool { return false },
		),
		measurement.NewPeriodic(
			logger,
			clock,
			time.Minute,
			appPushabilityMeasurement,
			measurement.NewResultSet(),
			allowedFailures.AppPushability,
			authFailedRetryFunc,
		),
		measurement.NewPeriodic(
			logger,
			clock,
			10*time.Second,
			recentLogsMeasurement,
			measurement.NewResultSet(),
			allowedFailures.RecentLogs,
			authFailedRetryFunc,
		),
		measurement.NewPeriodic(
			logger,
			clock,
			30*time.Second,
			streamingLogsMeasurement,
			measurement.NewResultSet(),
			allowedFailures.StreamingLogs,
			authFailedRetryFunc,
		),
	}
}

func createBufferedRunner() (cmdRunner.CmdRunner, *bytes.Buffer, *bytes.Buffer) {
	outBuf := bytes.NewBuffer([]byte{})
	errBuf := bytes.NewBuffer([]byte{})

	return cmdRunner.New(outBuf, errBuf, io.Copy), outBuf, errBuf
}

func logBufferedRunnerFailure(
	logger *log.Logger,
	whatFailed string,
	err error,
	outBuf, errBuf *bytes.Buffer,
) {
	logger.Printf(
		"Failed %s: %v\nstdout:\n%s\nstderr:\n%s\n",
		whatFailed,
		err,
		outBuf.String(),
		errBuf.String(),
	)
	outBuf.Reset()
	errBuf.Reset()
}

func tearDown(
	orc orchestrator.Orchestrator,
	orcCmdGenerator cfCmdGenerator.CfCmdGenerator,
	logger *log.Logger,
	pushWorkflow cfWorkflow.CfWorkflow,
	pushCmdGenerator cfCmdGenerator.CfCmdGenerator,
	runner cmdRunner.CmdRunner,
	runnerOutBuf *bytes.Buffer,
	runnerErrBuf *bytes.Buffer,
) {
	if err := orc.TearDown(runner, orcCmdGenerator); err != nil {
		logBufferedRunnerFailure(logger, "main teardown", err, runnerOutBuf, runnerErrBuf)
	}

	if err := runner.RunInSequence(pushWorkflow.TearDown(pushCmdGenerator)...); err != nil {
		logBufferedRunnerFailure(logger, "push workflow teardown", err, runnerOutBuf, runnerErrBuf)
	}
}
