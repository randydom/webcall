package main

import (
	"flag"
	"net/http"
	"fmt"
	"time"
	"os"
	"os/signal"
	"syscall"
	"sync"
	"strings"
	"strconv"
	"bufio"
	"runtime"
	"github.com/mehrvarz/webcall/skv"
	"github.com/mehrvarz/webcall/rkv"
	"gopkg.in/ini.v1"
	"github.com/lesismal/nbio/nbhttp"
	"github.com/lesismal/llib/std/crypto/tls"
	//"github.com/lesismal/nbio/taskpool"
	//"github.com/lesismal/nbio/nbhttp/websocket"
	_ "net/http/pprof"
// TODO do we want to use minify for html,css,js?
//	"github.com/tdewolff/minify/v2"
//	"github.com/tdewolff/minify/v2/css"
//	"github.com/tdewolff/minify/v2/html"
//	"github.com/tdewolff/minify/v2/js"
)

var version = flag.Bool("version", false, "show version")
var	builddate string

const configFileName = "config.ini"
const freeAccountTalkSecsConst = 3*60*60; // 3 hrs
const freeAccountServiceSecsConst = 3*24*60*60; // 3 days
const freeAccountBlockSecs = 7*24*60*60; // 7 days
const randomCallerWaitSecsConst = 1800
const randomCallerCallSecsConst = 600

var	db rkv.KV = nil

var dbName = "rtcsig.db"
const dbRegisteredIDs = "activeIDs" // internal name was changed active -> registered
const dbBlockedIDs = "blockedIDs"
const dbUserBucket = "userData2"

type CallerInfo struct { // for incoming calls
	AddrPort string // x.x.x.x:nnnn
	CallerName string
	CallTime int64
	CallerID string // the caller's calleeID for calling back
}
var dbCallsName = "rtccalls.db"
var	dbCalls rkv.KV = nil
const dbWaitingCaller = "waitingCallers"
const dbMissedCalls = "missedCalls"

var dbContactsName = "rtccontacts.db"
var	dbContacts rkv.KV = nil
const dbContactsBucket = "contacts" // calleeID -> map[callerID]name

var dbNotifName = "rtcnotif.db"
var	dbNotif rkv.KV = nil
const dbSentNotifTweets = "sentNotifTweets"

type PwIdCombo struct {
	Pw string
	CalleeId string
	Created int64
	Expiration int64
}
var dbHashedPwName = "rtchashedpw.db"
var	dbHashedPw rkv.KV = nil
const dbHashedPwBucket = "hashedpwbucket"

// main server; httpPort=8067 httpsPort=0/8068 wsPort=8071 wssPort=0/8443 turnPort=3739 dbName="rtcsig.db"
var hostname = "" //192.168.3.209
var httpPort = 8067
var httpsPort = 0 //8068
var httpToHttps = false
var wsPort = 8071
var wssPort = 0 //8443
var htmlPath = "webroot"
var insecureSkipVerify = false
var runTurn = false
var turnIP = "" //"66.228.46.43"
var turnPort = 3739
var turnDebugLevel = 3
var pprofPort = 0 //8980
var rtcdb = "" //"127.0.0.1" // will use port :8061 if no port is provided
var dbPath = "db/" // will only be used if rtcdb is empty

// twitter key for @WebCall user
var twitterKey = ""
var twitterSecret = ""

// web push keys for (TODO a copy of vapidPublicKey is also being used in settings.js)
var vapidPublicKey = ""
var vapidPrivateKey = ""
var adminEmail = ""

var readConfigLock sync.RWMutex
var wsUrl = ""
var wssUrl = ""

var	shutdownStarted rkv.AtomBool
var maintenanceMode = false
var allowNewAccounts = true
var disconnectCalleesWhenPeerConnected = false
var disconnectCallersWhenPeerConnected = true
var calleeClientVersion = ""
var freeAccountTalkSecs = freeAccountTalkSecsConst
var freeAccountServiceSecs = freeAccountServiceSecsConst

var hubMap map[string]*Hub
var hubMapMutex sync.RWMutex

var waitingCallerChanMap map[string]chan int // ip:port -> chan
var waitingCallerChanLock sync.RWMutex

var numberOfCallsToday = 0 // will be incremented by wshub.go processTimeValues()
var numberOfCallSecondsToday = 0
var numberOfCallsTodayMutex sync.RWMutex

var lastCurrentDayOfMonth = 0 // will be set by timer.go
var randomCallerWaitSecs = randomCallerWaitSecsConst
var randomCallerCallSecs = randomCallerCallSecsConst
var multiCallees = ""
var logevents = ""
var logeventMap map[string]bool
var logeventMutex sync.RWMutex
var calllog = ""

var httpRequestCountMutex sync.RWMutex
var httpRequestCount = 0
var httpResponseCount = 0
var httpResponseTime time.Duration

//var minifyerEnabled = false
//var minifyerObj *minify.M

var wsAddr string
var wssAddr string
var svr *nbhttp.Server
var svrs *nbhttp.Server

type wsClientDataType struct {
	hub *Hub
	dbEntry rkv.DbEntry
	dbUser rkv.DbUser
	calleeID string
}
var wsClientMap map[uint64]wsClientDataType
var wsClientMutex sync.RWMutex


func main() {
	flag.Parse()
	if *version {
		fmt.Printf("builddate %s\n",builddate)
		return
	}

	hubMap = make(map[string]*Hub) // calleeID -> *Hub
	waitingCallerChanMap = make(map[string]chan int)
	wsClientMap = make(map[uint64]wsClientDataType) // wsClientID -> wsClientData

	fmt.Printf("--------------- webcall startup ---------------\n")
	readConfig(true)
	outboundIP,err := rkv.GetOutboundIP()
	fmt.Printf("hostname=%s httpPort=%d httpsPort=%d outboundIP=%s\n",
		hostname, httpPort, httpsPort, outboundIP)
	fmt.Printf("wsPort=%d wsUrl=%s\n", wsPort, wsUrl)
	fmt.Printf("wssPort=%d wssUrl=%s\n", wssPort, wssUrl)
	fmt.Printf("runTurn=%v turnIP=%s\n", runTurn, turnIP)
	//fmt.Printf("dbName=%s dbCallsName=%s dbContactsName=%s dbNotifName=%s dbHashedPwName=%s\n",
	//	dbName, dbCallsName, dbContactsName, dbNotifName)

	if rtcdb=="" {
		db,err = skv.DbOpen(dbName,dbPath)
	} else {
		db,err = rkv.DbOpen(dbName,rtcdb)
	}
	if err!=nil {
		fmt.Printf("# error dbName %s open %v\n",dbName,err)
		return
	}
	err = db.CreateBucket(dbRegisteredIDs)
	if err!=nil {
		fmt.Printf("# error db %s create '%s' bucket err=%v\n",dbName,dbRegisteredIDs,err)
		db.Close()
		return
	}
	err = db.CreateBucket(dbBlockedIDs)
	if err!=nil {
		fmt.Printf("# error db %s create '%s' bucket err=%v\n",dbName,dbBlockedIDs,err)
		db.Close()
		return
	}
	err = db.CreateBucket(dbUserBucket)
	if err!=nil {
		fmt.Printf("# error db %s create '%s' bucket err=%v\n",dbName,dbUserBucket,err)
		db.Close()
		return
	}

	if rtcdb=="" {
		dbCalls,err = skv.DbOpen(dbCallsName,dbPath)
	} else {
		dbCalls,err = rkv.DbOpen(dbCallsName,rtcdb)
	}
	if err!=nil {
		fmt.Printf("# error dbCallsName %s open %v\n",dbCallsName,err)
		return
	}
	err = dbCalls.CreateBucket(dbWaitingCaller)
	if err!=nil {
		fmt.Printf("# error db %s create '%s' bucket err=%v\n",dbCallsName,dbWaitingCaller,err)
		dbCalls.Close()
		return
	}
	err = dbCalls.CreateBucket(dbMissedCalls)
	if err!=nil {
		fmt.Printf("# error db %s create '%s' bucket err=%v\n",dbCallsName,dbMissedCalls,err)
		dbCalls.Close()
		return
	}

	if rtcdb=="" {
		dbNotif,err = skv.DbOpen(dbNotifName,dbPath)
	} else {
		dbNotif,err = rkv.DbOpen(dbNotifName,rtcdb)
	}
	if err!=nil {
		fmt.Printf("# error dbNotifName %s open %v\n",dbNotifName,err)
		return
	}
	err = dbNotif.CreateBucket(dbSentNotifTweets)
	if err!=nil {
		fmt.Printf("# error db %s create '%s' bucket err=%v\n",dbNotifName,dbSentNotifTweets,err)
		dbNotif.Close()
		return
	}

	if rtcdb=="" {
		dbHashedPw,err = skv.DbOpen(dbHashedPwName,dbPath)
	} else {
		dbHashedPw,err = rkv.DbOpen(dbHashedPwName,rtcdb)
	}
	if err!=nil {
		fmt.Printf("# error dbHashedPwName %s open %v\n",dbHashedPwName,err)
		return
	}
	err = dbHashedPw.CreateBucket(dbHashedPwBucket)
	if err!=nil {
		fmt.Printf("# error db %s create '%s' bucket err=%v\n",dbHashedPwName,dbHashedPwBucket,err)
		dbHashedPw.Close()
		return
	}

	if rtcdb=="" {
		dbContacts,err = skv.DbOpen(dbContactsName,dbPath)
	} else {
		dbContacts,err = rkv.DbOpen(dbContactsName,rtcdb)
	}
	if err!=nil {
		fmt.Printf("# error dbContactsName %s open %v\n",dbContactsName,err)
		return
	}
	err = dbContacts.CreateBucket(dbContactsBucket)
	if err!=nil {
		fmt.Printf("# error db %s create '%s' bucket err=%v\n",dbContactsName,dbContactsBucket,err)
		dbContacts.Close()
		return
	}

	readStatsFile()

	// websocket handler
	if wsPort > 0 {
		wsAddr = fmt.Sprintf(":%d", wsPort)
		mux := &http.ServeMux{}
		mux.HandleFunc("/ws", serveWs)
		svr = nbhttp.NewServer(nbhttp.Config{
			Network: "tcp",
			Addrs: []string{wsAddr},
			MaxLoad: 1000000,				// TODO make configurable?
			ReleaseWebsocketPayload: true,	// TODO make configurable?
			NPoller: runtime.NumCPU() * 4,	// TODO make configurable? user workers?
		}, mux, nil)
		err = svr.Start()
		if err != nil {
			fmt.Printf("# nbio.Start wsPort failed: %v\n", err)
			return
		}
		defer svr.Stop()
	}
	if wssPort>0 {
		//var tlsConfig *tls.Config
		cer, err := tls.LoadX509KeyPair("tls.pem", "tls.key")
		if err != nil {
			fmt.Printf("# tls.LoadX509KeyPair err=(%v)\n", err)
			return
		}
		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{cer},
			InsecureSkipVerify: insecureSkipVerify,

			// Causes servers to use Go's default ciphersuite preferences,
			// which are tuned to avoid attacks. Does nothing on clients.
			PreferServerCipherSuites: true,
			// Only use curves which have assembly implementations
			CurvePreferences: []tls.CurveID{
				tls.CurveP256,
				tls.X25519,
			},

			MinVersion: tls.VersionTLS12,
			CipherSuites: []uint16{
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
				tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				// Best disabled, as they don't provide Forward Secrecy,
				// but might be necessary for some clients
				// tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
				// tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
			},
		}
		tlsConfig.BuildNameToCertificate()
		//fmt.Printf("tlsConfig %v\n", tlsConfig)

		wssAddr = fmt.Sprintf(":%d", wssPort)
		mux := &http.ServeMux{}
		mux.HandleFunc("/ws", serveWss)
		svrs = nbhttp.NewServerTLS(nbhttp.Config{
			Network: "tcp",
			Addrs: []string{wssAddr},
			MaxLoad: 1000000,				// TODO make configurable?
			ReleaseWebsocketPayload: true,	// TODO make configurable?
			NPoller: runtime.NumCPU() * 4,	// TODO make configurable? user workers?
		}, mux, nil, tlsConfig)

		err = svrs.Start()
		if err != nil {
			fmt.Printf("# nbio.Start wssPort failed: %v\n", err)
			return
		}
		defer svrs.Stop()
	}

	go httpServer()

	go runTurnServer()

	// periodically log stats
	go ticker30sec()

	// periodically call readConfig()
	go ticker10sec()

	// periodically check for remainingTalkSecs underruns
	go ticker2sec()

	// TODO make udpHealthPort configurable
	//go udpHealthService(8111)

	if pprofPort>0 {
		go func() {
			addr := fmt.Sprintf(":%d",pprofPort)
			fmt.Printf("starting pprofServer on %s\n",addr)
			pprofServer := &http.Server{Addr:addr}
			pprofServer.ListenAndServe()
		}()
	}

	time.Sleep(1 * time.Second)
	fmt.Printf("awaiting SIGTERM for shutdown...\n")
	sigc := make(chan os.Signal)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	<-sigc

	//////////////// shutdown //////////////////
	fmt.Printf("received os.Interrupt/SIGTERM signal: shutting down...\n")
	// shutdownStarted.Set(true) will end all timer routines
	// but it will not end our ListenAndServe() servers; this is why we use os.Exit() below
	shutdownStarted.Set(true)

	writeStatsFile()

	// wait a bit for shutdownStarted to take effect; then close all db's
	time.Sleep(2 * time.Second)

	fmt.Printf("dbContacts.Close...\n")
	err = dbContacts.Close()
	if err!=nil {
		fmt.Printf("# error db %s close err=%v\n",dbContactsName,err)
	}

	fmt.Printf("dbHashedPw.Close...\n")
	err = dbHashedPw.Close()
	if err!=nil {
		fmt.Printf("# error db %s close err=%v\n",dbHashedPwName,err)
	}

	fmt.Printf("dbNotif.Close...\n")
	err = dbNotif.Close()
	if err!=nil {
		fmt.Printf("# error db %s close err=%v\n",dbNotifName,err)
	}

	fmt.Printf("dbCalls.Close...\n")
	err = dbCalls.Close()
	if err!=nil {
		fmt.Printf("# error db %s close err=%v\n",dbCallsName,err)
	}

	fmt.Printf("db.Close...\n")
	err = db.Close()
	if err!=nil {
		fmt.Printf("# error db %s close err=%v\n",dbName,err)
	}

	if rtcdb!="" {
		err = rkv.Exit()
		if err!=nil {
			fmt.Printf("# error rkv.Exit err=%v\n",err)
		}
	}

	os.Exit(0)
}

// various utility functions
func getStats() string {
	// get number of total clients + number of active calls + number of active p2p/p2p-calls
	var numberOfOnlineCallees int64
	var numberOfOnlineCallers int64
	numberOfActivePureP2pCalls := 0
	hubMapMutex.RLock()
	for _,hub := range hubMap {
		numberOfOnlineCallees++
		hub.HubMutex.RLock()
		if hub.lastCallStartTime>0 && hub.CallerClient!=nil {
			numberOfOnlineCallers++
			if hub.LocalP2p && hub.RemoteP2p {
				numberOfActivePureP2pCalls++
			}
		}
		hub.HubMutex.RUnlock()
	}
	hubMapMutex.RUnlock()

	// this will show the total # of callees on all server instances
	var numberOfGlobalCallees int64
	var numberOfGlobalCallers int64
	if rtcdb=="" {
		numberOfGlobalCallees,numberOfGlobalCallers,_ = GetOnlineCalleeCount(true)
	} else {
		var err error
		numberOfGlobalCallees,numberOfGlobalCallers,err = rkv.GetOnlineCalleeCount(true)
		if err!=nil {
			fmt.Printf("# getStats GetOnlineCalleeCount err=%v\n", err)
		}
	}

	numberOfCallsTodayMutex.RLock()
	retStr := fmt.Sprintf("stats "+
		"loc:%d/%d/p%d "+
		"glob:%d/%d "+
		"callsToday:%d "+
		"callSecs:%d "+
		"gor:%d",
		numberOfOnlineCallees, numberOfOnlineCallers, numberOfActivePureP2pCalls,
		numberOfGlobalCallees, numberOfGlobalCallers,
		numberOfCallsToday,				// from hub.processTimeValues() TODO only for this server instance
		numberOfCallSecondsToday,		// from hub.processTimeValues() TODO only for this server instance
		runtime.NumGoroutine())
	numberOfCallsTodayMutex.RUnlock()
	return retStr
}

var locationGermanyForTime *time.Location = nil
func operationalNow() time.Time {
	if locationGermanyForTime == nil {
		// use german time
		loc, err := time.LoadLocation("Europe/Berlin")
		if err != nil {
			panic(err)
		}
		locationGermanyForTime = loc
	}

	// get the actual real time
	return time.Now().In(locationGermanyForTime)
}

func logWantedFor(topic string) bool {
	logeventMutex.RLock()
	if logeventMap[topic] {
		logeventMutex.RUnlock()
		return true
	}
	logeventMutex.RUnlock()
	return false
}

func readConfig(init bool) {
	//fmt.Printf("readConfig '%s' ...\n", configFileName)
	configIni, err := ini.Load(configFileName)
	if err != nil {
		configIni = nil
		//fmt.Printf("# ini file '%s' NOT found, err=%v\n", configFileName, err)
	} else {
		readConfigLock.Lock()

		if init {
			hostname = readIniString(configIni, "hostname", hostname, "")
			httpPort = readIniInt(configIni, "httpPort", httpPort, 8067, 1)
			httpsPort = readIniInt(configIni, "httpsPort", httpsPort, 0, 1)
			httpToHttps = readIniBoolean(configIni, "httpToHttps", httpToHttps, false)
			wsPort = readIniInt(configIni, "wsPort", wsPort, 8071, 1)
			wssPort = readIniInt(configIni, "wssPort", wssPort, 0, 1)
			htmlPath = readIniString(configIni, "htmlPath", htmlPath, "webroot")
			insecureSkipVerify = readIniBoolean(configIni, "insecureSkipVerify", insecureSkipVerify, false)
			runTurn = readIniBoolean(configIni, "runTurn", runTurn, false)
			turnIP = readIniString(configIni, "turnIP", turnIP, "")
			turnPort = readIniInt(configIni, "turnPort", turnPort, 3739, 1)
			pprofPort = readIniInt(configIni, "pprofPort", pprofPort, 0, 1) //8980

			rtcdb = readIniString(configIni, "rtcdb", rtcdb, "")
			if rtcdb!="" && strings.Index(rtcdb, ":") < 0 {
				rtcdb = rtcdb + ":8061"
			}

			dbPath = readIniString(configIni, "dbPath", dbPath, "db/")

			twitterKey = readIniString(configIni, "twitterKey", twitterKey, "")
			twitterSecret = readIniString(configIni, "twitterKey", twitterKey, "")

			vapidPublicKey = readIniString(configIni, "vapidPublicKey", vapidPublicKey, "")
			vapidPrivateKey = readIniString(configIni, "vapidPrivateKey", vapidPrivateKey, "")
		}

		maintenanceMode = readIniBoolean(configIni, "maintenanceMode", maintenanceMode, false)
		allowNewAccounts = readIniBoolean(configIni, "allowNewAccounts", allowNewAccounts, true)

		freeAccountTalkSecs = readIniInt(configIni, "freeAccountTalkHours",
			freeAccountTalkSecs, freeAccountTalkSecsConst, 60*60)
		freeAccountServiceSecs = readIniInt(configIni, "freeAccountServiceDays",
			freeAccountServiceSecs, freeAccountServiceSecsConst, 24*60*60)

		randomCallerWaitSecs = readIniInt(configIni, "randomCallerWaitSecs",
			randomCallerWaitSecs, randomCallerWaitSecsConst, 1)
		randomCallerCallSecs = readIniInt(configIni, "randomCallerCallSecs",
			randomCallerCallSecs, randomCallerCallSecsConst, 1)

		multiCallees = readIniString(configIni, "multiCallees", multiCallees, "")

		logevents = readIniString(configIni, "logevents", logevents, "")
		logeventSlice := strings.Split(logevents, ",")

		logeventMutex.Lock()
		logeventMap = make(map[string]bool)
		for _, s := range logeventSlice {
			logeventMap[strings.TrimSpace(s)] = true
		}
		logeventMutex.Unlock()

		// if *noPersist is true (used for benching), we don't write dbHashedPw
		//*noPersist = readIniBoolean(configIni, "nopersist", *noPersist, false)

		disconnectCalleesWhenPeerConnected = readIniBoolean(configIni,
			"disconnectCalleesWhenPeerConnected", disconnectCalleesWhenPeerConnected, false)

		disconnectCallersWhenPeerConnected = readIniBoolean(configIni,
			"disconnectCallersWhenPeerConnected", disconnectCallersWhenPeerConnected, true)

		calleeClientVersion = readIniString(configIni, "calleeClientVersion", calleeClientVersion, "")

		wsUrl = readIniString(configIni, "wsUrl", wsUrl, "")
		wssUrl = readIniString(configIni, "wssUrl", wssUrl, "")

		turnDebugLevel = readIniInt(configIni, "turnDebugLevel", turnDebugLevel, 3, 1)
		adminEmail = readIniString(configIni, "adminEmail", adminEmail, "")
		calllog = readIniString(configIni, "calllog", calllog, "")

		readConfigLock.Unlock()
	}
}

func readStatsFile() {
	statsIni, err := ini.Load(statsFileName)
	if err != nil {
		//fmt.Printf("# cannot read ini file '%s', err=%v\n", statsFileName, err)
	} else {
		iniKeyword := "numberOfCallsToday"
		iniValue,ok := readIniEntry(statsIni,iniKeyword)
		if ok {
			if iniValue=="" {
				numberOfCallsToday = 0
			} else {
				i64, err := strconv.ParseInt(iniValue, 10, 64)
				if err!=nil {
					fmt.Printf("# stats val %s: %s=%v err=%v\n",
						statsFileName, iniKeyword, iniValue, err)
				} else {
					//fmt.Printf("stats val %s: %s (%v) %v\n", statsFileName, iniKeyword, iniValue, i64)
					numberOfCallsToday = int(i64)
				}
			}
		}

		iniKeyword = "numberOfCallSecondsToday"
		iniValue,ok = readIniEntry(statsIni,iniKeyword)
		if ok {
			if iniValue=="" {
				numberOfCallsToday = 0
			} else {
				i64, err := strconv.ParseInt(iniValue, 10, 64)
				if err!=nil {
					fmt.Printf("# stats val %s: %s=%v err=%v\n",
						statsFileName, iniKeyword, iniValue, err)
				} else {
					//fmt.Printf("stats val %s: %s (%v) %v\n", statsFileName, iniKeyword, iniValue, i64)
					numberOfCallSecondsToday = int(i64)
				}
			}
		}

		iniKeyword = "lastCurrentDayOfMonth"
		iniValue,ok = readIniEntry(statsIni,iniKeyword)
		if ok {
			if iniValue=="" {
				lastCurrentDayOfMonth = 0
			} else {
				i64, err := strconv.ParseInt(iniValue, 10, 64)
				if err!=nil {
					fmt.Printf("# stats val %s: %s=%v err=%v\n",
						statsFileName, iniKeyword, iniValue, err)
				} else {
					//fmt.Printf("stats val %s: %s (%v) %v\n", statsFileName, iniKeyword, iniValue, i64)
					lastCurrentDayOfMonth = int(i64)
				}
			}
		}
	}
}

func writeStatsFile() {
	filename := statsFileName
	os.Remove(filename)
	file,err := os.OpenFile(filename,os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("# error creating statsFile (%s) err=%v\n", filename, err)
		return
	}
	fwr := bufio.NewWriter(file)
	defer func() {
		if fwr!=nil {
			fwr.Flush()
		}
		if file!=nil {
			if err := file.Close(); err != nil {
				fmt.Printf("# error closing statsFile (%s) err=%s\n",filename,err)
			}
		}
	}()

	numberOfCallsTodayMutex.RLock()
	data := fmt.Sprintf("numberOfCallsToday = %d\n"+
						"numberOfCallSecondsToday = %d\n"+
						"lastCurrentDayOfMonth = %d\n",
		numberOfCallsToday, numberOfCallSecondsToday, lastCurrentDayOfMonth)
	numberOfCallsTodayMutex.RUnlock()
	wrlen,err := fwr.WriteString(data)
	if err != nil {
		fmt.Printf("# error writing statsFile (%s) data err=%s\n", filename, err)
		return
	}
	if wrlen != len(data) {
		fmt.Printf("# error writing statsFile (%s) dlen=%d wrlen=%d\n",
			filename, len(data), wrlen)
		return
	}
	fmt.Printf("writing statsFile (%s) dlen=%d wrlen=%d\n",
		filename, len(data), wrlen)
	fwr.Flush()
}

