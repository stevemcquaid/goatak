package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kdudkov/goatak/cot"
	"github.com/spf13/viper"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/kdudkov/goatak/cot/v1"
	"github.com/kdudkov/goatak/model"
)

var (
	gitRevision            = "unknown"
	gitCommit              = "unknown"
	lastSeenOfflineTimeout = time.Minute * 2
)

type App struct {
	Logger  *zap.SugaredLogger
	tcpport int
	udpport int
	webport int

	lat  float64
	lon  float64
	zoom int8

	handlers sync.Map
	units    sync.Map

	ctx     context.Context
	uid     string
	ch      chan *v1.Msg
	logging bool
}

func NewApp(tcpport, udpport, webport int, logger *zap.SugaredLogger) *App {
	return &App{
		Logger:   logger,
		tcpport:  tcpport,
		udpport:  udpport,
		webport:  webport,
		ch:       make(chan *v1.Msg, 20),
		handlers: sync.Map{},
		units:    sync.Map{},
		uid:      uuid.New().String(),
	}
}

func (app *App) Run() {
	var cancel context.CancelFunc

	app.ctx, cancel = context.WithCancel(context.Background())

	go func() {
		if err := app.ListenUDP(fmt.Sprintf(":%d", app.udpport)); err != nil {
			panic(err)
		}
	}()

	go func() {
		if err := app.ListenTCP(fmt.Sprintf(":%d", app.tcpport)); err != nil {
			panic(err)
		}
	}()

	go func() {
		if err := NewHttp(app, fmt.Sprintf(":%d", app.webport)).Serve(); err != nil {
			panic(err)
		}
	}()

	go app.EventProcessor()
	go app.cleaner()

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)
	<-c
	app.Logger.Info("exiting...")
	cancel()
}

func (app *App) AddHandler(uid string, cl *ClientHandler) {
	app.Logger.Infof("new client: %s", uid)
	app.handlers.Store(uid, cl)
}

func (app *App) RemoveHandler(uid string) {
	if _, ok := app.handlers.Load(uid); ok {
		app.Logger.Infof("remove handler: %s", uid)
		app.handlers.Delete(uid)
	}
}

func (app *App) AddUnit(uid string, u *model.Unit) {
	if u == nil {
		return
	}
	app.units.Store(uid, u)
}

func (app *App) GetUnit(uid string) *model.Unit {
	if v, ok := app.units.Load(uid); ok {
		if unit, ok := v.(*model.Unit); ok {
			return unit
		} else {
			app.Logger.Errorf("invalid object for uid %s: %v", uid, v)
		}
	}
	return nil
}

func (app *App) Remove(uid string) {
	if _, ok := app.units.Load(uid); ok {
		app.units.Delete(uid)
	}
}

func (app *App) AddContact(uid string, u *model.Contact) {
	if u == nil {
		return
	}
	app.Logger.Infof("contact added %s", uid)
	app.units.Store(uid, u)
}

func (app *App) GetContact(uid string) *model.Contact {
	if v, ok := app.units.Load(uid); ok {
		if contact, ok := v.(*model.Contact); ok {
			return contact
		} else {
			app.Logger.Errorf("invalid object for uid %s: %v", uid, v)
		}
	}
	return nil
}

func (app *App) EventProcessor() {
	for {
		msg := <-app.ch

		if msg.TakMessage.CotEvent == nil {
			continue
		}

		if app.logging {
			if f, err := os.OpenFile(msg.GetType()+".log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666); err == nil {
				f.WriteString(msg.TakMessage.String())
				f.Close()
			} else {
				fmt.Println(err)
			}
		}

		switch {
		case msg.GetType() == "t-x-c-t":
			// ping
			app.Logger.Debugf("ping from %s", msg.GetUid())
			uid := msg.GetUid()
			if strings.HasSuffix(uid, "-ping") {
				uid = uid[:len(uid)-5]
			}
			if c := app.GetContact(uid); c != nil {
				c.SetLastSeenNow(nil)
			}
			app.SendTo(uid, cot.MakePong())
			app.SendToAllOther(msg.TakMessage, uid)
			continue
		case strings.HasPrefix(msg.GetType(), "a-"):
			app.Logger.Debugf("pos %s (%s) stale %s",
				msg.GetUid(),
				msg.GetCallsign(),
				msg.GetStale().Sub(time.Now()))
			if msg.IsContact() {
				if c := app.GetContact(msg.GetUid()); c != nil {
					c.SetLastSeenNow(msg.TakMessage)
				} else {
					app.AddContact(msg.GetUid(), model.ContactFromEvent(msg.TakMessage))
				}
			} else {
				app.AddUnit(msg.GetUid(), model.UnitFromEvent(msg.TakMessage))
			}
		case strings.HasPrefix(msg.GetType(), "b-"):
			app.Logger.Debugf("point %s (%s) stale %s",
				msg.GetUid(),
				msg.GetCallsign(),
				msg.GetStale().Sub(time.Now()))
			app.AddUnit(msg.GetUid(), model.UnitFromEvent(msg.TakMessage))
		default:
			app.Logger.Debugf("msg: %s", msg)
		}

		app.route(msg)
	}
}

func (app *App) route(msg *v1.Msg) {
	if len(msg.Detail.GetCallsignTo()) > 0 {
		for _, s := range msg.Detail.GetCallsignTo() {
			app.SendToCallsign(s, msg.TakMessage)
		}
	} else {
		app.SendToAllOther(msg.TakMessage, msg.GetUid())
	}
}

func (app *App) cleaner() {
	for {
		select {
		case <-time.Tick(time.Minute):
			app.cleanOldUnits()
		}
	}
}

func (app *App) cleanOldUnits() {
	toDelete := make([]string, 0)

	app.units.Range(func(key, value interface{}) bool {
		switch val := value.(type) {
		case *model.Unit:
			if val.IsOld() {
				toDelete = append(toDelete, key.(string))
				app.Logger.Debugf("removing %s", key)
			}

		case *model.Contact:
			if val.IsOld() {
				toDelete = append(toDelete, key.(string))
				app.Logger.Debugf("removing contact %s", key)
			} else {
				if val.IsOnline() && val.GetLastSeen().Add(lastSeenOfflineTimeout).Before(time.Now()) {
					val.SetOffline()
				}
			}
		}
		return true
	})

	for _, uid := range toDelete {
		app.units.Delete(uid)
	}
}

func (app *App) SendToAllOther(msg *v1.TakMessage, author string) {
	app.handlers.Range(func(key, value interface{}) bool {
		if key.(string) != author {
			value.(*ClientHandler).AddMsg(msg)
		}
		return true
	})
}

func (app *App) SendTo(uid string, msg *v1.TakMessage) {
	if h, ok := app.handlers.Load(uid); ok {
		h.(*ClientHandler).AddMsg(msg)
	}
}

func (app *App) SendToCallsign(callsign string, msg *v1.TakMessage) {
	app.handlers.Range(func(key, value interface{}) bool {
		h := value.(*ClientHandler)
		if h.GetCallsign() == callsign {
			h.AddMsg(msg)
			return false
		}
		return true
	})
}

func main() {
	fmt.Printf("version %s %s\n", gitRevision, gitCommit)
	var logging = flag.Bool("logging", false, "save all events to files")
	var conf = flag.String("config", "goatak_server.yml", "name of config file")
	flag.Parse()

	viper.SetConfigFile(*conf)

	viper.SetDefault("web_port", 8080)
	viper.SetDefault("tcp_port", 8999)
	viper.SetDefault("udp_port", 8999)

	viper.SetDefault("me.lat", 35.462939)
	viper.SetDefault("me.lon", -97.537283)
	viper.SetDefault("me.zoom", 5)

	err := viper.ReadInConfig()
	if err != nil {
		panic(fmt.Errorf("Fatal error config file: %s \n", err))
	}

	flag.Parse()

	cfg := zap.NewDevelopmentConfig()
	logger, _ := cfg.Build()
	defer logger.Sync()

	app := NewApp(
		viper.GetInt("tcp_port"),
		viper.GetInt("udp_port"),
		viper.GetInt("web_port"),
		logger.Sugar(),
	)
	app.logging = *logging
	app.lat = viper.GetFloat64("me.lat")
	app.lon = viper.GetFloat64("me.lon")
	app.zoom = int8(viper.GetInt("me.zoom"))
	app.Run()
}
