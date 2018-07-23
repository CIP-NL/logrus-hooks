package airbrake // import "gopkg.in/gemnasium/logrus-airbrake-hook.v3"

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/airbrake/gobrake"
	"github.com/sirupsen/logrus"
)

// AirbrakeHook to send exceptions to an exception-tracking service compatible
// with the Airbrake API.
type airbrakeHook struct {
	Airbrake *gobrake.Notifier
}

// NewHook Returns a new Airbrake hook given the projectID, apiKey and environment
func NewHook(projectID int64, apiKey, env string) *airbrakeHook {
	airbrake := gobrake.NewNotifier(projectID, apiKey)
	airbrake.AddFilter(func(notice *gobrake.Notice) *gobrake.Notice {
		if env == "development" {
			return nil
		}
		notice.Context["environment"] = env
		return notice
	})
	hook := &airbrakeHook{
		Airbrake: airbrake,
	}
	return hook
}

func (hook *airbrakeHook) Fire(entry *logrus.Entry) error {
	var notifyErr error
	err, ok := entry.Data["error"].(error)
	if ok {
		notifyErr = err
	} else {
		notifyErr = errors.New(entry.Message)
	}
	var req *http.Request
	for k, v := range entry.Data {
		if r, ok := v.(*http.Request); ok {
			req = r
			delete(entry.Data, k)
			break
		}
	}
	notice := hook.Airbrake.Notice(notifyErr, req, 3)
	for k, v := range entry.Data {
		notice.Context[k] = fmt.Sprintf("%s", v)
	}

	hook.sendNotice(notice)
	return nil
}

func (hook *airbrakeHook) sendNotice(notice *gobrake.Notice) {
	if _, err := hook.Airbrake.SendNotice(notice); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to send error to Airbrake: %v\n", err)
	}
}

func (hook *airbrakeHook) Levels() []logrus.Level {
	return []logrus.Level{
		logrus.ErrorLevel,
		logrus.FatalLevel,
		logrus.PanicLevel,
	}
}

// LogAttempt used to test error messages
// func LogAttempt(projectID int64, testAPIKey string, testEnv string) {
// 	log := logrus.New()
// 	log.Level = logrus.DebugLevel
// 	log.AddHook(NewHook(projectID, testAPIKey, testEnv))
// 	log.Error("Bitcoin price: 0")
// }