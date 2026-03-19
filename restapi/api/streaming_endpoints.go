package api

import (
	"io"
	"net/http"
	"strings"

	"github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/instruments"
	"github.com/danielpaulus/go-ios/ios/syslog"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// Notifications uses instruments to get application state change events. It will stream the events as json objects separated by line breaks until it errors out.
// Listen                godoc
// @Summary      uses instruments to get application state change events
// @Description uses instruments to get application state change events
// @Tags         general
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Router       /notifications [get]
func Notifications(c *gin.Context) {
	device := c.MustGet(IOS_KEY).(ios.DeviceEntry)
	listenerFunc, closeFunc, err := instruments.ListenAppStateNotifications(device)
	if err != nil {
		log.Fatal(err)
	}
	c.Stream(func(w io.Writer) bool {

		notification, err := listenerFunc()
		if err != nil {
			c.JSON(http.StatusInternalServerError, err)
			closeFunc()
			return false
		}

		_, err = w.Write([]byte(MustMarshal(notification)))

		if err != nil {
			c.JSON(http.StatusInternalServerError, err)
			closeFunc()
			return false
		}
		w.Write([]byte("\n"))
		return true
	})

}

// Syslog
// Listen                godoc
// @Summary      Uses SSE to connect to the LISTEN command
// @Description Uses SSE to connect to the LISTEN command
// @Tags         general
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Router       /listen [get]
func parseFilterKeywords(filterStr string) []string {
	if filterStr == "" {
		return nil
	}
	parts := strings.Split(filterStr, ",")
	var keywords []string
	for _, p := range parts {
		kw := strings.TrimSpace(p)
		if kw != "" {
			keywords = append(keywords, kw)
		}
	}
	return keywords
}

func Syslog(c *gin.Context) {
	log.Info("connect")
	device := c.MustGet(IOS_KEY).(ios.DeviceEntry)
	syslogConnection, err := syslog.New(device)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err})
		return
	}
	filterKeywords := parseFilterKeywords(c.Query("filter"))
	c.Stream(func(w io.Writer) bool {
		m, _ := syslogConnection.ReadLogMessage()
		if !syslog.LineMatchesKeywords(m, filterKeywords) {
			return true
		}
		w.Write([]byte(MustMarshal(m)))
		return true
	})
}

// Listen send server side events when devices are plugged in or removed
// Listen                godoc
// @Summary      Uses SSE to connect to the LISTEN command
// @Description Uses SSE to connect to the LISTEN command
// @Tags         general
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Router       /listen [get]
func Listen(c *gin.Context) {
	// We are streaming current time to clients in the interval 10 seconds
	log.Info("connect")
	a, _, _ := ios.Listen()
	c.Stream(func(w io.Writer) bool {
		l, _ := a()
		// Stream message to client from message channel
		w.Write([]byte(MustMarshal(l)))
		return true
	})
}
