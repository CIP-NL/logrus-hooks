package sentry

import (
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/getsentry/raven-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

var (
	severityMap = map[logrus.Level]raven.Severity{
		logrus.DebugLevel: raven.DEBUG,
		logrus.InfoLevel:  raven.INFO,
		logrus.WarnLevel:  raven.WARNING,
		logrus.ErrorLevel: raven.ERROR,
		logrus.FatalLevel: raven.FATAL,
		logrus.PanicLevel: raven.FATAL,
	}
)

// Hook delivers logs to a sentry server.
type Hook struct {
	// Timeout sets the time to wait for a delivery error from the sentry server.
	// If this is set to zero the server will not wait for any response and will
	// consider the message correctly sent.
	//
	// This is ignored for asynchronous hooks. If you want to set a timeout when
	// using an async hook (to bound the length of time that hook.Flush can take),
	// you probably want to create your own raven.Client and set
	// ravenClient.Transport.(*raven.HTTPTransport).Client.Timeout to set a
	// timeout on the underlying HTTP request instead.
	Timeout                 time.Duration
	StacktraceConfiguration StackTraceConfiguration

	client *raven.Client
	levels []logrus.Level

	serverName   string
	ignoreFields map[string]struct{}
	extraFilters map[string]func(interface{}) interface{}

	asynchronous bool

	mu sync.RWMutex
	wg sync.WaitGroup
}

// The Stacktracer interface allows an error type to return a raven.Stacktrace.
type Stacktracer interface {
	GetStacktrace() *raven.Stacktrace
}

type causer interface {
	Cause() error
}

type pkgErrorStackTracer interface {
	StackTrace() errors.StackTrace
}

// StackTraceConfiguration allows for configuring stacktraces
type StackTraceConfiguration struct {
	// whether stacktraces should be enabled
	Enable bool
	// the level at which to start capturing stacktraces
	Level logrus.Level
	// how many stack frames to skip before stacktrace starts recording
	Skip int
	// the number of lines to include around a stack frame for context
	Context int
	// the prefixes that will be matched against the stack frame.
	// if the stack frame's package matches one of these prefixes
	// sentry will identify the stack frame as "in_app"
	InAppPrefixes []string
	// whether sending exception type should be enabled.
	SendExceptionType bool
	// whether the exception type and message should be switched.
	SwitchExceptionTypeAndMessage bool
}

// Factory function to create proper hook
func New(dsn string) *Hook {
	hook, err := NewHook(dsn, []logrus.Level{
		logrus.PanicLevel,
		logrus.FatalLevel,
		logrus.ErrorLevel,
	})
	if err == nil {
		return hook
	}
	return nil
}

//Checks if the DSN is correct and if the sentry server is accessible
func (hook *Hook) Verify(dsn string) bool {
	_, err := NewHook(dsn, []logrus.Level{
		logrus.PanicLevel,
		logrus.FatalLevel,
		logrus.ErrorLevel,
	})
	return err == nil
}

// NewHook creates a hook to be added to an instance of logger
// and initializes the raven client.
// This method sets the timeout to 100 milliseconds.
func NewHook(DSN string, levels []logrus.Level) (*Hook, error) {
	client, err := raven.New(DSN)
	if err != nil {
		return nil, err
	}
	return NewWithClientHook(client, levels)
}

// NewWithTagsHook creates a hook with tags to be added to an instance
// of logger and initializes the raven client. This method sets the timeout to
// 100 milliseconds.
func NewWithTagsHook(DSN string, tags map[string]string, levels []logrus.Level) (*Hook, error) {
	client, err := raven.NewWithTags(DSN, tags)
	if err != nil {
		return nil, err
	}
	return NewWithClientHook(client, levels)
}

// NewWithClientHook creates a hook using an initialized raven client.
// This method sets the timeout to 100 milliseconds.
func NewWithClientHook(client *raven.Client, levels []logrus.Level) (*Hook, error) {
	return &Hook{
		Timeout: 100 * time.Millisecond,
		StacktraceConfiguration: StackTraceConfiguration{
			Enable:            false,
			Level:             logrus.ErrorLevel,
			Skip:              5,
			Context:           0,
			InAppPrefixes:     nil,
			SendExceptionType: true,
		},
		client:       client,
		levels:       levels,
		ignoreFields: make(map[string]struct{}),
		extraFilters: make(map[string]func(interface{}) interface{}),
	}, nil
}

// NewAsyncHook creates a hook same as NewHook, but in asynchronous
// mode.
func NewAsyncHook(DSN string, levels []logrus.Level) (*Hook, error) {
	hook, err := NewHook(DSN, levels)
	return setAsync(hook), err
}

// NewAsyncWithTagsHook creates a hook same as NewWithTagsHook, but
// in asynchronous mode.
func NewAsyncWithTagsHook(DSN string, tags map[string]string, levels []logrus.Level) (*Hook, error) {
	hook, err := NewWithTagsHook(DSN, tags, levels)
	return setAsync(hook), err
}

// NewAsyncWithClientHook creates a hook same as NewWithClientHook,
// but in asynchronous mode.
func NewAsyncWithClientHook(client *raven.Client, levels []logrus.Level) (*Hook, error) {
	hook, err := NewWithClientHook(client, levels)
	return setAsync(hook), err
}

func setAsync(hook *Hook) *Hook {
	if hook == nil {
		return nil
	}
	hook.asynchronous = true
	return hook
}

// Fire is called when an event should be sent to sentry
// Special fields that sentry uses to give more information to the server
// are extracted from entry.Data (if they are found)
// These fields are: error, logger, server_name, http_request, tags
func (hook *Hook) Fire(entry *logrus.Entry) error {
	hook.mu.RLock() // Allow multiple go routines to log simultaneously
	defer hook.mu.RUnlock()
	packet := raven.NewPacket(entry.Message)
	packet.Timestamp = raven.Timestamp(entry.Time)
	packet.Level = severityMap[entry.Level]
	packet.Platform = "go"

	df := newDataField(entry.Data)

	// set special fields
	if hook.serverName != "" {
		packet.ServerName = hook.serverName
	}
	if logger, ok := df.getLogger(); ok {
		packet.Logger = logger
	}
	if serverName, ok := df.getServerName(); ok {
		packet.ServerName = serverName
	}
	if eventID, ok := df.getEventID(); ok {
		packet.EventID = eventID
	}
	if tags, ok := df.getTags(); ok {
		packet.Tags = tags
	}
	if fingerprint, ok := df.getFingerprint(); ok {
		packet.Fingerprint = fingerprint
	}
	if req, ok := df.getHTTPRequest(); ok {
		packet.Interfaces = append(packet.Interfaces, req)
	}
	if user, ok := df.getUser(); ok {
		packet.Interfaces = append(packet.Interfaces, user)
	}

	// set stacktrace data
	stConfig := &hook.StacktraceConfiguration
	if stConfig.Enable && entry.Level <= stConfig.Level {
		if err, ok := df.getError(); ok {
			var currentStacktrace *raven.Stacktrace
			currentStacktrace = hook.findStacktrace(err)
			if currentStacktrace == nil {
				currentStacktrace = raven.NewStacktrace(stConfig.Skip, stConfig.Context, stConfig.InAppPrefixes)
			}
			cause := errors.Cause(err)
			if cause == nil {
				cause = err
			}
			exc := raven.NewException(cause, currentStacktrace)
			if !stConfig.SendExceptionType {
				exc.Type = ""
			}
			if stConfig.SwitchExceptionTypeAndMessage {
				packet.Interfaces = append(packet.Interfaces, currentStacktrace)
				packet.Culprit = exc.Type + ": " + currentStacktrace.Culprit()
			} else {
				packet.Interfaces = append(packet.Interfaces, exc)
				packet.Culprit = err.Error()
			}
		} else {
			currentStacktrace := raven.NewStacktrace(stConfig.Skip, stConfig.Context, stConfig.InAppPrefixes)
			if currentStacktrace != nil {
				packet.Interfaces = append(packet.Interfaces, currentStacktrace)
			}
		}
	} else {
		// set the culprit even when the stack trace is disabled, as long as we have an error
		if err, ok := df.getError(); ok {
			packet.Culprit = err.Error()
		}
	}

	// set other fields
	dataExtra := hook.formatExtraData(df)
	if packet.Extra == nil {
		packet.Extra = dataExtra
	} else {
		for k, v := range dataExtra {
			packet.Extra[k] = v
		}
	}

	_, errCh := hook.client.Capture(packet, nil)

	if hook.asynchronous {
		// Our use of hook.mu guarantees that we are following the WaitGroup rule of
		// not calling Add in parallel with Wait.
		hook.wg.Add(1)
		go func() {
			if err := <-errCh; err != nil {
				fmt.Println(err)
			}
			hook.wg.Done()
		}()
		return nil
	} else if timeout := hook.Timeout; timeout == 0 {
		return nil
	} else {
		timeoutCh := time.After(timeout)
		select {
		case err := <-errCh:
			return err
		case <-timeoutCh:
			return fmt.Errorf("no response from sentry server in %s", timeout)
		}
	}
}

// Flush waits for the log queue to empty. This function only does anything in
// asynchronous mode.
func (hook *Hook) Flush() {
	if !hook.asynchronous {
		return
	}
	hook.mu.Lock() // Claim exclusive access; any logging goroutines will block until the flush completes
	defer hook.mu.Unlock()

	hook.wg.Wait()
}

func (hook *Hook) findStacktrace(err error) *raven.Stacktrace {
	var stacktrace *raven.Stacktrace
	var stackErr errors.StackTrace
	for err != nil {
		// Find the earliest *raven.Stacktrace, or error.StackTrace
		if tracer, ok := err.(Stacktracer); ok {
			stacktrace = tracer.GetStacktrace()
			stackErr = nil
		} else if tracer, ok := err.(pkgErrorStackTracer); ok {
			stacktrace = nil
			stackErr = tracer.StackTrace()
		}
		if cause, ok := err.(causer); ok {
			err = cause.Cause()
		} else {
			break
		}
	}
	if stackErr != nil {
		stacktrace = hook.convertStackTrace(stackErr)
	}
	return stacktrace
}

// convertStackTrace converts an errors.StackTrace into a natively consumable
// *raven.Stacktrace
func (hook *Hook) convertStackTrace(st errors.StackTrace) *raven.Stacktrace {
	stConfig := &hook.StacktraceConfiguration
	stFrames := []errors.Frame(st)
	frames := make([]*raven.StacktraceFrame, 0, len(stFrames))
	for i := range stFrames {
		pc := uintptr(stFrames[i])
		fn := runtime.FuncForPC(pc)
		file, line := fn.FileLine(pc)
		frame := raven.NewStacktraceFrame(pc, file, line, stConfig.Context, stConfig.InAppPrefixes)
		if frame != nil {
			frames = append(frames, frame)
		}
	}

	// Sentry wants the frames with the oldest first, so reverse them
	for i, j := 0, len(frames)-1; i < j; i, j = i+1, j-1 {
		frames[i], frames[j] = frames[j], frames[i]
	}
	return &raven.Stacktrace{Frames: frames}
}

// Levels returns the available logging levels.
func (hook *Hook) Levels() []logrus.Level {
	return hook.levels
}

// SetRelease sets release tag.
func (hook *Hook) SetRelease(release string) {
	hook.client.SetRelease(release)
}

// SetEnvironment sets environment tag.
func (hook *Hook) SetEnvironment(environment string) {
	hook.client.SetEnvironment(environment)
}

// SetServerName sets server_name tag.
func (hook *Hook) SetServerName(serverName string) {
	hook.serverName = serverName
}

// AddIgnore adds field name to ignore.
func (hook *Hook) AddIgnore(name string) {
	hook.ignoreFields[name] = struct{}{}
}

// AddExtraFilter adds a custom filter function.
func (hook *Hook) AddExtraFilter(name string, fn func(interface{}) interface{}) {
	hook.extraFilters[name] = fn
}

func (hook *Hook) formatExtraData(df *dataField) (result map[string]interface{}) {
	// create a map for passing to Sentry's extra data
	result = make(map[string]interface{}, df.len())
	for k, v := range df.data {
		if df.isOmit(k) {
			continue // skip already used special fields
		}
		if _, ok := hook.ignoreFields[k]; ok {
			continue
		}

		if fn, ok := hook.extraFilters[k]; ok {
			v = fn(v) // apply custom filter
		} else {
			v = formatData(v) // use default formatter
		}
		result[k] = v
	}
	return result
}

// formatData returns value as a suitable format.
func formatData(value interface{}) (formatted interface{}) {
	switch value := value.(type) {
	case json.Marshaler:
		return value
	case error:
		return value.Error()
	case fmt.Stringer:
		return value.String()
	default:
		return value
	}
}

// Sends a dummy error to the server
func logAttempt(DSN string) {
	log := logrus.New()
	hook, err := NewHook(DSN, []logrus.Level{
		logrus.PanicLevel,
		logrus.FatalLevel,
		logrus.ErrorLevel,
	})

	if err == nil {
		log.Hooks.Add(hook)
	}
	log.Error("Everything is going wrong in testing")
}
