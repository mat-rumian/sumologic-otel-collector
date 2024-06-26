// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

//go:build windows

// This is a copy of https://github.com/open-telemetry/opentelemetry-collector/blob/20710ff2aeaed3fe1c3e46423291522674db44d6/otelcol/collector_windows.go.
// This is required to maintain our command line flags when running the collector as a Windows service, until either
// the necessary names become public, or there's a better way of customizing config providers.

package main // import "go.opentelemetry.io/collector/otelcol"

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/confmap/converter/expandconverter"
	"go.opentelemetry.io/collector/confmap/provider/envprovider"
	"go.opentelemetry.io/collector/confmap/provider/fileprovider"
	"go.opentelemetry.io/collector/confmap/provider/httpprovider"
	"go.opentelemetry.io/collector/confmap/provider/httpsprovider"
	"go.opentelemetry.io/collector/confmap/provider/yamlprovider"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"

	"go.opentelemetry.io/collector/featuregate"
	"go.opentelemetry.io/collector/otelcol"

	"github.com/SumoLogic/sumologic-otel-collector/pkg/configprovider/globprovider"
)

type windowsService struct {
	settings otelcol.CollectorSettings
	col      *otelcol.Collector
	flags    *flag.FlagSet
}

// NewSvcHandler constructs a new svc.Handler using the given CollectorSettings.
func NewSvcHandler(set otelcol.CollectorSettings) svc.Handler {
	return &windowsService{settings: set, flags: flags(featuregate.GlobalRegistry())}
}

// Execute implements https://godoc.org/golang.org/x/sys/windows/svc#Handler
func (s *windowsService) Execute(args []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	// The first argument supplied to service.Execute is the service name. If this is
	// not provided for some reason, raise a relevant error to the system event log
	if len(args) == 0 {
		return false, 1213 // 1213: ERROR_INVALID_SERVICENAME
	}

	elog, err := openEventLog(args[0])
	if err != nil {
		return false, 1501 // 1501: ERROR_EVENTLOG_CANT_START
	}

	colErrorChannel := make(chan error, 1)

	changes <- svc.Status{State: svc.StartPending}
	if err = s.start(elog, colErrorChannel); err != nil {
		_ = elog.Error(3, fmt.Sprintf("failed to start service: %v", err))
		return false, 1064 // 1064: ERROR_EXCEPTION_IN_SERVICE
	}
	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for req := range requests {
		switch req.Cmd {
		case svc.Interrogate:
			changes <- req.CurrentStatus

		case svc.Stop, svc.Shutdown:
			changes <- svc.Status{State: svc.StopPending}
			if err = s.stop(colErrorChannel); err != nil {
				_ = elog.Error(3, fmt.Sprintf("errors occurred while shutting down the service: %v", err))
			}
			changes <- svc.Status{State: svc.Stopped}
			return false, 0

		default:
			_ = elog.Error(3, fmt.Sprintf("unexpected service control request #%d", req.Cmd))
			return false, 1052 // 1052: ERROR_INVALID_SERVICE_CONTROL
		}
	}

	return false, 0
}

func (s *windowsService) start(elog *eventlog.Log, colErrorChannel chan error) error {
	// Append to new slice instead of the already existing s.settings.LoggingOptions slice to not change that.
	_ = elog.Info(6666, "starting windows service")
	s.settings.LoggingOptions = append(
		[]zap.Option{zap.WrapCore(withWindowsCore(elog))},
		s.settings.LoggingOptions...,
	)
	// Parse all the flags manually.
	buf := bytes.NewBufferString("")
	s.flags.SetOutput(buf)
	s.flags.PrintDefaults()
	_ = elog.Info(6667, fmt.Sprintf("usage: %s", buf.String()))

	// Parse all the flags manually.
	_ = elog.Info(6668, "parsing arguments")
	if err := s.flags.Parse(os.Args[1:]); err != nil {
		_ = elog.Info(6669, fmt.Sprintf("error parsing argument: %v", err))
		return err
	}

	_ = elog.Info(6670, "new collector with flags")
	var err error
	err = updateSettingsUsingFlags(&s.settings, s.flags)
	if err != nil {
		return err
	}
	s.col, err = otelcol.NewCollector(s.settings)
	if err != nil {
		return err
	}

	// col.Run blocks until receiving a SIGTERM signal, so needs to be started
	// asynchronously, but it will exit early if an error occurs on startup
	go func() {
		colErrorChannel <- s.col.Run(context.Background())
	}()

	// wait until the collector server is in the Running state
	go func() {
		for {
			state := s.col.GetState()
			if state == otelcol.StateRunning {
				colErrorChannel <- nil
				break
			}
			time.Sleep(time.Millisecond * 200)
		}
	}()

	// wait until the collector server is in the Running state, or an error was returned
	return <-colErrorChannel
}

func (s *windowsService) stop(colErrorChannel chan error) error {
	s.col.Shutdown()
	// return the response of col.Start
	return <-colErrorChannel
}

func openEventLog(serviceName string) (*eventlog.Log, error) {
	elog, err := eventlog.Open(serviceName)
	if err != nil {
		return nil, fmt.Errorf("service failed to open event log: %w", err)
	}

	return elog, nil
}

var _ zapcore.Core = (*windowsEventLogCore)(nil)

type windowsEventLogCore struct {
	core    zapcore.Core
	elog    *eventlog.Log
	encoder zapcore.Encoder
}

func (w windowsEventLogCore) Enabled(level zapcore.Level) bool {
	return w.core.Enabled(level)
}

func (w windowsEventLogCore) With(fields []zapcore.Field) zapcore.Core {
	enc := w.encoder.Clone()
	for _, field := range fields {
		field.AddTo(enc)
	}
	return windowsEventLogCore{
		core:    w.core,
		elog:    w.elog,
		encoder: enc,
	}
}

func (w windowsEventLogCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if w.Enabled(ent.Level) {
		return ce.AddCore(ent, w)
	}
	return ce
}

func (w windowsEventLogCore) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	buf, err := w.encoder.EncodeEntry(ent, fields)
	if err != nil {
		_ = w.elog.Warning(2, fmt.Sprintf("failed encoding log entry %v\r\n", err))
		return err
	}
	msg := buf.String()
	buf.Free()

	switch ent.Level {
	case zapcore.FatalLevel, zapcore.PanicLevel, zapcore.DPanicLevel:
		// golang.org/x/sys/windows/svc/eventlog does not support Critical level event logs
		return w.elog.Error(3, msg)
	case zapcore.ErrorLevel:
		return w.elog.Error(3, msg)
	case zapcore.WarnLevel:
		return w.elog.Warning(2, msg)
	case zapcore.InfoLevel:
		return w.elog.Info(1, msg)
	}
	// We would not be here if debug were disabled so log as info to not drop.
	return w.elog.Info(1, msg)
}

func (w windowsEventLogCore) Sync() error {
	return w.core.Sync()
}

func withWindowsCore(elog *eventlog.Log) func(zapcore.Core) zapcore.Core {
	return func(core zapcore.Core) zapcore.Core {
		encoderConfig := zap.NewProductionEncoderConfig()
		encoderConfig.LineEnding = "\r\n"
		return windowsEventLogCore{core, elog, zapcore.NewConsoleEncoder(encoderConfig)}
	}
}

func updateSettingsUsingFlags(set *otelcol.CollectorSettings, flags *flag.FlagSet) error {
	resolverSet := &set.ConfigProviderSettings.ResolverSettings
	configFlags := getConfigFlag(flags)

	if len(configFlags) > 0 {
		resolverSet.URIs = configFlags
	}
	if len(resolverSet.URIs) == 0 {
		return errors.New("at least one config flag must be provided")
	}
	// Provide a default set of providers and converters if none have been specified.
	// TODO: Remove this after CollectorSettings.ConfigProvider is removed and instead
	// do it in the builder.
	if len(resolverSet.ProviderFactories) == 0 && len(resolverSet.ConverterFactories) == 0 {
		set.ConfigProviderSettings = newDefaultConfigProviderSettings(resolverSet.URIs)
	}
	return nil
}

func newDefaultConfigProviderSettings(uris []string) otelcol.ConfigProviderSettings {
	return otelcol.ConfigProviderSettings{
		ResolverSettings: confmap.ResolverSettings{
			URIs: uris,
			ProviderFactories: []confmap.ProviderFactory{
				fileprovider.NewFactory(),
				envprovider.NewFactory(),
				yamlprovider.NewFactory(),
				httpprovider.NewFactory(),
				httpsprovider.NewFactory(),
				globprovider.NewFactory(),
			},
			ConverterFactories: []confmap.ConverterFactory{expandconverter.NewFactory()},
		},
	}
}
