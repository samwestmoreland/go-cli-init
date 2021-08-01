// Package cli contains initialisation functions for go-flags and go-logging.
// It facilitates sharing them between several projects.
package logging

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/crypto/ssh/terminal"
	"gopkg.in/op/go-logging.v1"
)

var log = MustGetLogger()

// A Verbosity is used as a flag to define logging verbosity.
type Verbosity logging.Level

// MaxVerbosity is the maximum verbosity we support.
const MaxVerbosity Verbosity = Verbosity(logging.DEBUG)

// MinVerbosity is the maximum verbosity we support.
const MinVerbosity Verbosity = Verbosity(logging.ERROR)

// An open file handle for file-based logging.
var logFile *os.File

// fileAttempts is the number of log files we attempt to open before giving up.
const fileAttempts = 10

// UnmarshalFlag implements flag parsing.
// It accepts input in three forms:
// As an integer level, -v 4 (where -v 1 == warning & error only)
// As a named level, -v debug
// As a series of flags, -vvv (but note that bare -v does *not* work)
func (v *Verbosity) UnmarshalFlag(in string) error {
	in = strings.ToLower(in)
	switch strings.ToLower(in) {
	case "critical", "fatal":
		*v = Verbosity(logging.CRITICAL)
		return nil
	case "0", "error":
		*v = Verbosity(logging.ERROR)
		return nil
	case "1", "warning", "warn":
		*v = Verbosity(logging.WARNING)
		return nil
	case "2", "notice", "v":
		*v = Verbosity(logging.NOTICE)
		return nil
	case "3", "info", "vv":
		*v = Verbosity(logging.INFO)
		return nil
	case "4", "debug", "vvv":
		*v = Verbosity(logging.DEBUG)
		return nil
	}
	if i, err := strconv.Atoi(in); err == nil {
		return v.fromInt(i)
	} else if c := strings.Count(in, "v"); len(in) == c {
		return v.fromInt(c)
	}
	return fmt.Errorf("Invalid log level %s", in)
}

func (v *Verbosity) fromInt(i int) error {
	if i < 0 {
		log.Warning("Invalid log level %d; minimum is 0. Displaying critical errors only.")
		*v = Verbosity(logging.CRITICAL)
		return nil
	}
	log.Warning("Invalid log level %d; maximum is 4. Displaying all messages.")
	*v = Verbosity(logging.DEBUG)
	return nil
}

// An Options contains various logging-related options.
type Options struct {
	Verbosity     Verbosity `short:"v" long:"verbosity" description:"Verbosity of output (error, warning, notice, info, debug)" default:"warning"`
	File          string    `long:"file" description:"File to echo full logging output to"`
	FileVerbosity Verbosity `long:"file_verbosity" description:"Log level for file output" default:"debug"`
	Append        bool      `long:"append" description:"Append log to existing file instead of overwriting its content. If not set, a new file will be chosen if the existing one is already open."`
	Colour        bool      `long:"colour" description:"Forces coloured output."`
	NoColour      bool      `long:"nocolour" description:"Forces colourless output."`
	Structured    bool      `long:"structured_logs" env:"STRUCTURED_LOGS" description:"Output logs in structured (JSON) format"`
}

// InitLoggingOptions initialises logging from the given options struct.
func InitLoggingOptions(opts *Options) (LogLevelInfo, error) {
	info := initialiseLogging(opts.Verbosity, opts.Structured, opts.Colour, opts.NoColour)
	if opts.File == "" {
		return info, nil
	}
	f, err := initLogFile(opts.File, opts.Append)
	if err != nil {
		return info, err
	}
	logFile = f
	logging.SetBackend(logInfo.backend, initLogging(opts.FileVerbosity, f, opts.Structured, false, true))
	return info, nil
}

// MustInitLoggingOptions is like InitLoggingOptions but dies on error.
func MustInitLoggingOptions(opts *Options) LogLevelInfo {
	info, err := InitLoggingOptions(opts)
	if err != nil {
		log.Fatalf("Failed to open log file: %s", err)
	}
	return info
}

// InitLoggingOptionsLike initialises logging from a struct that is assignable to
// an Options. This is useful for a caller to customise their own flags.
// It panics if the struct is not assignable.
func InitLoggingOptionsLike(optionsLike interface{}) (LogLevelInfo, error) {
	v := reflect.ValueOf(optionsLike)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	var opts Options
	reflect.ValueOf(&opts).Elem().Set(v)
	return InitLoggingOptions(&opts)
}

// openLogFile opens a file as a logging backend.
// If append is true, it always opens the given filename and appends to it.
// If not, it will attempt to find an unlocked log file starting from the given name and use that.
func initLogFile(filename string, append bool) (*os.File, error) {
	if err := os.MkdirAll(path.Dir(filename), os.ModeDir|0755); err != nil {
		return nil, err
	}
	if append {
		f, err := os.OpenFile(filename, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0664)
		if err != nil {
			return nil, err
		}
		// Grab an advisory lock on the file. Note that in this path it's best-effort only.
		syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		return f, nil
	}
	// If we're not appending, try to find an available log file.
	for i := 0; i < fileAttempts; i++ {
		// N.B. Don't want to truncate here because someone else might be writing.
		f, err := os.OpenFile(logFileName(filename, i), os.O_RDWR|os.O_CREATE, 0664)
		if err != nil {
			return nil, err
		}
		// See if we can acquire a lock. If not we will try another file.
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			// Locking this file failed (EWOULDBLOCK is the most likely)
			f.Close()
			continue
		}
		// OK, we have the lock, now reset the file.
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, err
		} else if _, err := f.Seek(0, 0); err != nil {
			f.Close()
			return nil, err
		}
		return f, nil
	}
	return nil, fmt.Errorf("Failed to acquire a log file after %d attempts", fileAttempts)
}

func logFileName(base string, attempt int) string {
	if attempt == 0 {
		return base
	}
	return base + "." + strconv.Itoa(attempt)
}

// InitLogging initialises logging backends.
func InitLogging(verbosity Verbosity) LogLevelInfo {
	return initialiseLogging(verbosity, false, false, false)
}

func initialiseLogging(verbosity Verbosity, structured, coloured, colourless bool) LogLevelInfo {
	backend := initLogging(verbosity, os.Stderr, structured, coloured, colourless)
	logging.SetBackend(backend)
	logInfo.backend = backend
	return &logInfo
}

// InitFileLogging initialises logging backends, both to stderr and to a file.
// If the file path is empty then it will be ignored.
func InitFileLogging(stderrVerbosity, fileVerbosity Verbosity, filename string) (LogLevelInfo, error) {
	return InitStructuredLogging(stderrVerbosity, fileVerbosity, filename, false)
}

// MustInitFileLogging is like InitFileLogging but dies on any errors.
func MustInitFileLogging(stderrVerbosity, fileVerbosity Verbosity, filename string) LogLevelInfo {
	return MustInitStructuredLogging(stderrVerbosity, fileVerbosity, filename, false)
}

// InitStructuredLogging is like InitFileLogging but allows specifying whether the output should be
// structured as JSON.
func InitStructuredLogging(stderrVerbosity, fileVerbosity Verbosity, filename string, structured bool) (LogLevelInfo, error) {
	return InitLoggingOptions(&Options{
		Verbosity:     stderrVerbosity,
		File:          filename,
		FileVerbosity: fileVerbosity,
		Structured:    structured,
	})
}

// MustInitStructuredLogging is like InitStructuredLogging but dies on any errors.
func MustInitStructuredLogging(stderrVerbosity, fileVerbosity Verbosity, filename string, structured bool) LogLevelInfo {
	info, err := InitStructuredLogging(stderrVerbosity, fileVerbosity, filename, structured)
	if err != nil {
		log.Fatalf("Failed to open log file: %s", err)
	}
	return info
}

// CloseFileLogging closes any open file-based logging (i.e. anything opened via
// a call to InitFileLogging or its friends).
func CloseFileLogging() error {
	if logFile == nil {
		return nil
	}
	return logFile.Close()
}

func initLogging(verbosity Verbosity, out *os.File, structured, coloured, colourless bool) logging.LeveledBackend {
	level := logging.Level(verbosity)
	backend := logging.NewLogBackend(out, "", 0)
	backendFormatted := logging.NewBackendFormatter(backend, logFormatter(out, structured, coloured, colourless))
	backendLeveled := logging.AddModuleLevel(backendFormatted)
	backendLeveled.SetLevel(level, "")
	return backendLeveled
}

func logFormatter(f *os.File, structured, coloured, colourless bool) logging.Formatter {
	if structured {
		return jsonFormatter{}
	}
	formatStr := "%{time:15:04:05.000} %{level:7s}: %{message}"
	if shouldColour(f, coloured, colourless) {
		formatStr = "%{color}" + formatStr + "%{color:reset}"
	}
	return logging.MustStringFormatter(formatStr)
}

// shouldColour returns whether we should show coloured output for the given file.
func shouldColour(f *os.File, coloured, colourless bool) bool {
	if coloured {
		return true
	} else if colourless {
		return false
	}
	return terminal.IsTerminal(int(f.Fd()))
}

// getLoggerName returns the name of the calling package as a logger name (e.g. "github.com.peterebden.cli")
func getLoggerName(skip int) string {
	_, file, _, ok := runtime.Caller(skip)
	if !ok {
		return "<unknown>" // Shouldn't really happen but best to handle it.
	}
	return strings.Replace(strings.TrimPrefix(path.Dir(file), ".go"), "/", ".", -1)
}

// MustGetLogger is a wrapper around go-logging's function of the same name. It automatically determines a logger name.
// The logger is registered and will be returned by ModuleLevels().
func MustGetLogger() *logging.Logger {
	return MustGetLoggerNamed(getLoggerName(2)) // 2 to skip back to the calling function.
}

// MustGetLoggerNamed is like MustGetLogger but lets the caller choose the name.
func MustGetLoggerNamed(name string) *logging.Logger {
	logInfo.Register(name)
	return logging.MustGetLogger(name)
}

// A LogLevelInfo describes and can modify levels of the set of registered loggers.
type LogLevelInfo interface {
	// ModuleLevels returns the level of all loggers retrieved by MustGetLogger().
	ModuleLevels() map[string]logging.Level
	// SetLevel modifies the level of a specific logger.
	SetLevel(level logging.Level, module string)
}

type logLevelInfo struct {
	backend logging.LeveledBackend
	modules map[string]struct{}
	mutex   sync.Mutex
}

func (info *logLevelInfo) Register(name string) {
	info.mutex.Lock()
	defer info.mutex.Unlock()
	info.modules[name] = struct{}{}
}

func (info *logLevelInfo) ModuleLevels() map[string]logging.Level {
	info.mutex.Lock()
	defer info.mutex.Unlock()
	levels := map[string]logging.Level{}
	levels[""] = info.backend.GetLevel("")
	for module := range info.modules {
		levels[module] = info.backend.GetLevel(module)
	}
	return levels
}

func (info *logLevelInfo) SetLevel(level logging.Level, module string) {
	info.backend.SetLevel(level, module)
}

var logInfo = logLevelInfo{modules: map[string]struct{}{}}

type jsonFormatter struct{}

func (f jsonFormatter) Format(calldepth int, r *logging.Record, w io.Writer) error {
	fn := ""
	pc, file, line, ok := runtime.Caller(calldepth + 1)
	if !ok {
		file = "???"
		line = 0
	}
	if f := runtime.FuncForPC(pc); f != nil {
		fn = f.Name()
	}
	return json.NewEncoder(w).Encode(&jsonEntry{
		File:   fmt.Sprintf("%s:%d", file, line),
		Func:   fn,
		Level:  jsonLevelNames[r.Level],
		Module: r.Module,
		Time:   r.Time.Format(jsonTimestampFormat),
		Msg:    r.Message(),
	})
}

var jsonLevelNames = []string{
	"critical",
	"error",
	"warning",
	"notice",
	"info",
	"debug",
}

type jsonEntry struct {
	File   string `json:"file"`
	Func   string `json:"func"`
	Level  string `json:"level"`
	Module string `json:"module"`
	Msg    string `json:"msg"`
	Time   string `json:"time"`
}

const jsonTimestampFormat = "2006-01-02T15:04:05.000Z07:00"
