//go:build linux
// +build linux

// Package logging is a minimal logger that avoids third-party dependencies.
// CNI plugins must write diagnostics to stderr (captured by kubelet / containerd).
package logging

import (
	"io"
	"log"
	"os"
	"strings"
)

const (
	LevelError = iota
	LevelWarning
	LevelInfo
	LevelDebug
	LevelVerbose
)

var (
	logger = log.New(os.Stderr, "sriov-cni: ", log.LstdFlags|log.Lmicroseconds)
	level  = LevelError
)

// Set sets the log level and output file.
func Set(logLevel, logFile string) {
	if logLevel == "" {
		return
	}
	switch strings.ToLower(logLevel) {
	case "error":
		level = LevelError
	case "warning", "warn":
		level = LevelWarning
	case "info":
		level = LevelInfo
	case "debug":
		level = LevelDebug
	case "verbose":
		level = LevelVerbose
	}

	if logFile != "" {
		f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			Errorf("cannot open logfile %s: %v", logFile, err)
			return
		}
		var w io.Writer = f
		if level >= LevelDebug {
			// Also keep stderr to ease troubleshooting from the kubectl side
			w = io.MultiWriter(os.Stderr, f)
		}
		logger.SetOutput(w)
	}
}

func Errorf(format string, a ...interface{}) { logger.Printf("[error] "+format, a...) }
func Warnf(format string, a ...interface{}) {
	if level >= LevelWarning {
		logger.Printf("[warning] "+format, a...)
	}
}
func Infof(format string, a ...interface{}) {
	if level >= LevelInfo {
		logger.Printf("[info] "+format, a...)
	}
}
func Debugf(format string, a ...interface{}) {
	if level >= LevelDebug {
		logger.Printf("[debug] "+format, a...)
	}
}
func Verbosef(format string, a ...interface{}) {
	if level >= LevelVerbose {
		logger.Printf("[verbose] "+format, a...)
	}
}
