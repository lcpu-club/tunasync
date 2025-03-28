package worker

import (
	"bytes"
	"errors"
	"html/template"
	"path/filepath"
	"time"
)

// mirror provider is the wrapper of mirror jobs

const (
	_WorkingDirKey = "working_dir"
	_LogDirKey     = "log_dir"
	_LogFileKey    = "log_file"
)

// A mirrorProvider instance
type mirrorProvider interface {
	// name
	Name() string
	Upstream() string
	Description() string

	Type() providerEnum

	// Start then Wait
	Run(started chan empty) error
	// Start the job
	Start() error
	// Wait job to finish
	Wait() error
	// terminate mirror job
	Terminate() error
	// job hooks
	IsRunning() bool
	// Cgroup
	Cgroup() *cgroupHook
	// ZFS
	ZFS() *zfsHook
	// Docker
	Docker() *dockerHook

	AddHook(hook jobHook)
	Hooks() []jobHook

	Interval() time.Duration
	Retry() int
	Timeout() time.Duration

	WorkingDir() string
	LogDir() string
	LogFile() string
	IsMaster() bool
	DataSize() string

	// enter context
	EnterContext() *Context
	// exit context
	ExitContext() *Context
	// return context
	Context() *Context

	// set in newMirrorProvider, used by cmdJob.Wait
	SetSuccessExitCodes(codes []int)
	GetSuccessExitCodes() []int
}

// newProvider creates a mirrorProvider instance
// using a mirrorCfg and the global cfg
func newMirrorProvider(mirror mirrorConfig, cfg *Config) mirrorProvider {

	formatLogDir := func(logDir string, m mirrorConfig) string {
		tmpl, err := template.New("logDirTmpl-" + m.Name).Parse(logDir)
		if err != nil {
			panic(err)
		}
		var formatedLogDir bytes.Buffer
		tmpl.Execute(&formatedLogDir, m)
		return formatedLogDir.String()
	}

	logDir := mirror.LogDir
	mirrorDir := mirror.MirrorDir
	if logDir == "" {
		logDir = cfg.Global.LogDir
	}
	if mirrorDir == "" {
		mirrorDir = filepath.Join(
			cfg.Global.MirrorDir, mirror.MirrorSubDir, mirror.Name,
		)
	}
	if mirror.Interval == 0 {
		mirror.Interval = cfg.Global.Interval
	}
	if mirror.Retry == 0 {
		mirror.Retry = cfg.Global.Retry
	}
	if mirror.Timeout == 0 {
		mirror.Timeout = cfg.Global.Timeout
	}
	logDir = formatLogDir(logDir, mirror)

	// IsMaster
	isMaster := true
	if mirror.Role == "slave" {
		isMaster = false
	} else {
		if mirror.Role != "" && mirror.Role != "master" {
			logger.Warningf("Invalid role configuration for %s", mirror.Name)
		}
	}

	var provider mirrorProvider

	switch mirror.Provider {
	case provCommand:
		pc := cmdConfig{
			name:        mirror.Name,
			description: mirror.Description,
			upstreamURL: mirror.Upstream,
			command:     mirror.Command,
			workingDir:  mirrorDir,
			failOnMatch: mirror.FailOnMatch,
			sizePattern: mirror.SizePattern,
			logDir:      logDir,
			logFile:     filepath.Join(logDir, "latest.log"),
			interval:    time.Duration(mirror.Interval) * time.Minute,
			retry:       mirror.Retry,
			timeout:     time.Duration(mirror.Timeout) * time.Second,
			env:         mirror.Env,
		}
		p, err := newCmdProvider(pc)
		if err != nil {
			panic(err)
		}
		p.isMaster = isMaster
		provider = p
	case provRsync:
		rc := rsyncConfig{
			name:              mirror.Name,
			description:       mirror.Description,
			upstreamURL:       mirror.Upstream,
			rsyncCmd:          mirror.Command,
			username:          mirror.Username,
			password:          mirror.Password,
			excludeFile:       mirror.ExcludeFile,
			extraOptions:      mirror.RsyncOptions,
			rsyncNeverTimeout: mirror.RsyncNoTimeo,
			rsyncTimeoutValue: mirror.RsyncTimeout,
			globalOptions:     cfg.Global.RsyncOptions,
			overriddenOptions: mirror.RsyncOverride,
			useOverrideOnly:   mirror.RsyncOverrideOnly,
			rsyncEnv:          mirror.Env,
			workingDir:        mirrorDir,
			logDir:            logDir,
			logFile:           filepath.Join(logDir, "latest.log"),
			useIPv6:           mirror.UseIPv6,
			useIPv4:           mirror.UseIPv4,
			interval:          time.Duration(mirror.Interval) * time.Minute,
			retry:             mirror.Retry,
			timeout:           time.Duration(mirror.Timeout) * time.Second,
		}
		p, err := newRsyncProvider(rc)
		if err != nil {
			panic(err)
		}
		p.isMaster = isMaster
		provider = p
	case provTwoStageRsync:
		rc := twoStageRsyncConfig{
			name:               mirror.Name,
			description:        mirror.Description,
			stage1Profile:      mirror.Stage1Profile,
			stage1ExtraOptions: mirror.Stage1ExtraOptions,
			upstreamURL:        mirror.Upstream,
			rsyncCmd:           mirror.Command,
			username:           mirror.Username,
			password:           mirror.Password,
			excludeFile:        mirror.ExcludeFile,
			extraOptions:       mirror.RsyncOptions,
			rsyncNeverTimeout:  mirror.RsyncNoTimeo,
			rsyncTimeoutValue:  mirror.RsyncTimeout,
			rsyncEnv:           mirror.Env,
			workingDir:         mirrorDir,
			logDir:             logDir,
			logFile:            filepath.Join(logDir, "latest.log"),
			useIPv6:            mirror.UseIPv6,
			useIPv4:            mirror.UseIPv4,
			interval:           time.Duration(mirror.Interval) * time.Minute,
			retry:              mirror.Retry,
			timeout:            time.Duration(mirror.Timeout) * time.Second,
		}
		p, err := newTwoStageRsyncProvider(rc)
		if err != nil {
			panic(err)
		}
		p.isMaster = isMaster
		provider = p
	default:
		panic(errors.New("Invalid mirror provider"))
	}

	// Add Logging Hook
	provider.AddHook(newLogLimiter(provider))

	// Add ZFS Hook
	if cfg.ZFS.Enable {
		provider.AddHook(newZfsHook(provider, cfg.ZFS.Zpool))
	}

	// Add Btrfs Snapshot Hook
	if cfg.BtrfsSnapshot.Enable {
		provider.AddHook(newBtrfsSnapshotHook(provider, cfg.BtrfsSnapshot.SnapshotPath, mirror))
	}

	// Add Docker Hook
	if cfg.Docker.Enable && len(mirror.DockerImage) > 0 {
		provider.AddHook(newDockerHook(provider, cfg.Docker, mirror))

	} else if cfg.Cgroup.Enable {
		// Add Cgroup Hook
		provider.AddHook(
			newCgroupHook(
				provider, cfg.Cgroup, mirror.MemoryLimit,
			),
		)
	}

	addHookFromCmdList := func(cmdList []string, execOn uint8) {
		if execOn != execOnSuccess && execOn != execOnFailure {
			panic("Invalid option for exec-on")
		}
		for _, cmd := range cmdList {
			h, err := newExecPostHook(provider, execOn, cmd)
			if err != nil {
				logger.Errorf("Error initializing mirror %s: %s", mirror.Name, err.Error())
				panic(err)
			}
			provider.AddHook(h)
		}
	}

	// ExecOnSuccess hook
	if len(mirror.ExecOnSuccess) > 0 {
		addHookFromCmdList(mirror.ExecOnSuccess, execOnSuccess)
	} else {
		addHookFromCmdList(cfg.Global.ExecOnSuccess, execOnSuccess)
	}
	addHookFromCmdList(mirror.ExecOnSuccessExtra, execOnSuccess)

	// ExecOnFailure hook
	if len(mirror.ExecOnFailure) > 0 {
		addHookFromCmdList(mirror.ExecOnFailure, execOnFailure)
	} else {
		addHookFromCmdList(cfg.Global.ExecOnFailure, execOnFailure)
	}
	addHookFromCmdList(mirror.ExecOnFailureExtra, execOnFailure)

	successExitCodes := []int{}
	if cfg.Global.SuccessExitCodes != nil {
		successExitCodes = append(successExitCodes, cfg.Global.SuccessExitCodes...)
	}
	if mirror.SuccessExitCodes != nil {
		successExitCodes = append(successExitCodes, mirror.SuccessExitCodes...)
	}
	if len(successExitCodes) > 0 {
		logger.Infof("Non-zero success exit codes set for mirror %s: %v", mirror.Name, successExitCodes)
		provider.SetSuccessExitCodes(successExitCodes)
	}

	return provider
}
