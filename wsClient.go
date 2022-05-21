// WebCall Copyright 2022 timur.mobi. All rights reserved.
//
// Method serve() is the Websocket handler for http-to-ws upgrade.
// Method receiveProcess() is the Websocket signaling handler.
// KeepAliveMgr takes care of keeping ws-clients connected.

package main

import (
	"bytes"
	"time"
	"strings"
	"fmt"
	"strconv"
	"errors"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"sync"
	"github.com/mehrvarz/webcall/atombool"
	"github.com/lesismal/nbio/nbhttp/websocket"
)

const (
	pingPeriod = 60
	// we send a ping to the client when we didn't hear from it for pingPeriod secs
	// when we send a ping, we set the time for our next ping in pingPeriod secs after that
	// whenever we receive something from the client (data or a ping or a pong)
	// we reset the time for our next ping to be sent in pingPeriod secs after that moment
	// when pingPeriod expires, it means that we didn't hear from the client for pingPeriod secs
	// so we send our ping
	// and we set SetReadDeadline bc we expect to receive a pong in response within max 30s
	// if there is still no response from the client by then, we consider the client to be dead
	// in other words: we cap the connection if we don't hear from a client for pingPeriod + 30 secs

	// browser clients do not send pings, so it is only the server sending pings
	// new: android clients do not send pings anymore (for powermgmt reasons)
	// now outdated:
	//   android clients send pings to the server every 60 secs and we respond with pongs
	//   since the pingPeriod of android clients is shorter than that of this server,
	//   this server will in practice not send any pings to android clients
	//   say an android client sends a ping, the server sends a pong and shortly after the client reboots
	//   the server will wait for 90s without receiving anything from this client
	//   after 90s the server will send a ping to check the client
	//   after another 20s the server declares the client dead - 100s after the clients last ping
)

var keepAliveMgr *KeepAliveMgr
var ErrWriteNotConnected = errors.New("Write not connected")

type WsClient struct {
	hub *Hub
	wsConn *websocket.Conn
	isOnline atombool.AtomBool	// connected to signaling server
	isConnectedToPeer atombool.AtomBool // before pickup
	isMediaConnectedToPeer atombool.AtomBool // after pickup
	pickupSent atombool.AtomBool
	calleeInitReceived atombool.AtomBool
	callerOfferForwarded atombool.AtomBool
	RemoteAddr string // with port
	RemoteAddrNoPort string // no port
	userAgent string // ws UA
	calleeID string
	globalCalleeID string // unique calleeID for multiCallees as key for hubMap[]
	connType string
	callerID string
	callerName string
	clientVersion string
	callerTextMsg string
	pingSent uint64
	pongReceived uint64
	pongSent uint64
	pingReceived uint64
	authenticationShown bool // whether to show "pion auth for client (%v) SUCCESS"
	isCallee bool
	clearOnCloseDone bool
}

func serveWs(w http.ResponseWriter, r *http.Request) {
	serve(w, r, false)
}

func serveWss(w http.ResponseWriter, r *http.Request) {
	serve(w, r, true)
}

func serve(w http.ResponseWriter, r *http.Request, tls bool) {
	if logWantedFor("wsverbose") {
		fmt.Printf("serve url=%s tls=%v\n", r.URL.String(), tls)
	}

	if keepAliveMgr==nil {
		keepAliveMgr = NewKeepAliveMgr()
		go keepAliveMgr.Run()
	}

	remoteAddr := r.RemoteAddr
	realIpFromRevProxy := r.Header.Get("X-Real-Ip")
	if realIpFromRevProxy!="" {
		remoteAddr = realIpFromRevProxy
	}

	remoteAddrNoPort := remoteAddr
	idxPort := strings.Index(remoteAddrNoPort,":")
	if idxPort>=0 {
		remoteAddrNoPort = remoteAddrNoPort[:idxPort]
	}

	var wsClientID64 uint64 = 0
	var wsClientData wsClientDataType
	url_arg_array, ok := r.URL.Query()["wsid"]
	if !ok || len(url_arg_array[0]) <= 0{
		return
	}
	wsClientIDstr := strings.ToLower(url_arg_array[0])
	wsClientID64, _ = strconv.ParseUint(wsClientIDstr, 10, 64)
	if wsClientID64<=0 {
		// not valid
		fmt.Printf("# serveWs invalid wsClientIDstr=%s %s url=%s\n",
			wsClientIDstr, remoteAddr, r.URL.String())
		return
	}
	wsClientMutex.Lock()
	wsClientData,ok = wsClientMap[wsClientID64]
	if ok {
		// ensure wsClientMap[wsClientID64] will not be removed
		wsClientData.removeFlag = false
		wsClientMap[wsClientID64] = wsClientData
	}
	wsClientMutex.Unlock()
	if !ok {
		// this callee has just exited
		// does not to be logged
		//fmt.Printf("serveWs ws=%d does not exist %s url=%s\n",
		//	wsClientID64, remoteAddr, r.URL.String())
		// TODO why does r.URL start with "//": url=//timur.mobi:8443/ws?wsid=47639023704
		return
	}

	callerID := ""
	url_arg_array, ok = r.URL.Query()["callerId"]
	if ok && len(url_arg_array[0]) > 0 {
		callerID = strings.ToLower(url_arg_array[0])
	}

	callerName := ""
	url_arg_array, ok = r.URL.Query()["name"]
	if ok && len(url_arg_array[0]) > 0 {
		callerName = url_arg_array[0]
	}
	//fmt.Printf("serve callerID=%s (%v)\n", callerID, r.URL.Query())

	upgrader := websocket.NewUpgrader()
	//upgrader.EnableCompression = true // TODO
	upgrader.CheckOrigin = func(r *http.Request) bool {
		return true
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Printf("# Upgrade err=%v\n", err)
		return
	}
	wsConn := conn.(*websocket.Conn)
	//wsConn.EnableWriteCompression(true) // TODO

	// the only time browser clients can be expected to send anything, is after we sent a ping
	// this is why we set NO read deadline here; we do it when we send a ping
	wsConn.SetReadDeadline(time.Time{})

	client := &WsClient{wsConn:wsConn}
	client.calleeID = wsClientData.calleeID // this is the local ID
	client.globalCalleeID = wsClientData.globalID
	client.clientVersion = wsClientData.clientVersion
	client.callerID = callerID
	client.callerName = callerName
	if tls {
		client.connType = "serveWss"
	} else {
		client.connType = "serveWs"
	}

	keepAliveMgr.Add(wsConn)
	// set the time for sending the next ping
	keepAliveMgr.SetPingDeadline(wsConn, pingPeriod, client) // now + pingPeriod secs

	client.isOnline.Set(true)
	client.RemoteAddr = remoteAddr
	client.RemoteAddrNoPort = remoteAddrNoPort
	client.userAgent = r.UserAgent()
	client.authenticationShown = false // being used to make sure 'TURN auth SUCCESS' is only shown 1x per client

	hub := wsClientData.hub // set by /login wsClientMap[wsClientID] = wsClientDataType{...}
	client.hub = hub

	upgrader.OnMessage(func(wsConn *websocket.Conn, messageType websocket.MessageType, data []byte) {
		// clear read deadline for now; we set it again when we send the next ping
		wsConn.SetReadDeadline(time.Time{})
		// set the time for sending the next ping
		// so whenever client sends some data, we postpone our next ping by pingPeriod secs
		keepAliveMgr.SetPingDeadline(wsConn, pingPeriod, client) // now + pingPeriod secs

		switch messageType {
		case websocket.TextMessage:
			//fmt.Println("TextMessage:", messageType, string(data), len(data))
			n := len(data)
			if n>0 {
				if logWantedFor("wsreceive") {
					max := n; if max>20 { max = 20 }
					fmt.Printf("%s (%s) received n=%d isCallee=%v (%s)\n",
						client.connType, client.calleeID, n, client.isCallee, data[:max])
				}
				client.receiveProcess(data, wsConn)
			}
		case websocket.BinaryMessage:
			fmt.Printf("# %s binary dataLen=%d\n", client.connType, len(data))
		}
	})

	upgrader.SetPongHandler(func(wsConn *websocket.Conn, s string) {
		// we received a pong from the client
		if logWantedFor("gotpong") {
			fmt.Printf("gotPong (%s)\n",client.calleeID)
		}
		// clear read deadline for now; we set it again when we send the next ping
		wsConn.SetReadDeadline(time.Time{})
		// set the time for sending the next ping: now + pingPeriod secs
		keepAliveMgr.SetPingDeadline(wsConn, pingPeriod, client) // now + pingPeriod secs
		client.pongReceived++
	})

	upgrader.SetPingHandler(func(wsConn *websocket.Conn, s string) {
		// we received a ping from the client
		if logWantedFor("gotping") {
			fmt.Printf("gotPing (%s)\n",client.calleeID)
		}
		client.pingReceived++
		// clear read deadline for now; we set it again when we send the next ping
		wsConn.SetReadDeadline(time.Time{})
		// set the time for sending the next ping: now + pingPeriod secs
		keepAliveMgr.SetPingDeadline(wsConn, pingPeriod, client) // now + pingPeriod secs
		// send the pong
		wsConn.WriteMessage(websocket.PongMessage, nil)
		atomic.AddInt64(&pongSentCounter, 1)
		client.pongSent++
	})

	wsConn.OnClose(func(c *websocket.Conn, err error) {
		keepAliveMgr.Delete(c)
		client.isOnline.Set(false) // prevent close() from closing this already closed connection
		if logWantedFor("wsclose") {
			if err!=nil {
				fmt.Printf("%s (%s) onclose isCallee=%v err=%v\n",
					client.connType, client.calleeID, client.isCallee, err)
			} else {
				fmt.Printf("%s (%s) onclose isCallee=%v noerr\n",
					client.connType, client.calleeID, client.isCallee)
			}
		}
		if client.isCallee && client.isConnectedToPeer.Get() {
			client.peerConHasEnded("OnClose")
// TODO tmtmtm call? -> client.hub.CalleeClient.peerConHasEnded("OnClose")
// we must do this if "onClose isCallee==false" can happen
		}
		if err!=nil {
			client.hub.doUnregister(client, "OnClose "+err.Error())
		} else {
			client.hub.doUnregister(client, "OnClose")
		}
	})

	if hub.CalleeClient==nil {
		// callee client (1st client)
		if logWantedFor("wsclient") {
			fmt.Printf("%s (%s) callee conn ws=%d %s\n", client.connType,
				client.calleeID, wsClientID64, client.RemoteAddr)
		}
		client.isCallee = true
		client.calleeInitReceived.Set(false)

		hub.HubMutex.Lock()
		hub.IsCalleeHidden = wsClientData.dbUser.Int2&1!=0
		hub.IsUnHiddenForCallerAddr = ""
		hub.WsClientID = wsClientID64
		hub.CalleeClient = client
		hub.CallerClient = nil
		hub.ServiceStartTime = time.Now().Unix()
		hub.ConnectedToPeerSecs = 0
		hub.HubMutex.Unlock()

		if !strings.HasPrefix(client.calleeID,"random") {
			// get values related to talk- and service-time for this callee from the db
			// so that 1s-ticker can calculate the live remaining time
			hub.ServiceStartTime = wsClientData.dbEntry.StartTime // race?
			hub.ConnectedToPeerSecs = int64(wsClientData.dbUser.ConnectedToPeerSecs)
		}
		hub.CallDurationSecs = 0
		//fmt.Printf("%s talkSecs=%d startTime=%d serviceSecs=%d\n",
		//	client.connType, hub.ConnectedToPeerSecs, hub.ServiceStartTime, hub.ServiceDurationSecs)
	} else if hub.CallerClient==nil {
		// caller client (2nd client)
		if logWantedFor("wsclient") {
			//callerID := ""
			//if client.hub!=nil && client.hub.CallerClient!=nil {
			//	callerID = client.hub.CallerClient.callerID
			//}
			fmt.Printf("%s (%s) caller conn ws=%d (%s) %s\n", client.connType, client.calleeID,
				wsClientID64, callerID, client.RemoteAddr)
		}

		client.isCallee = false
		client.callerOfferForwarded.Set(false)
		hub.HubMutex.Lock()
		hub.CallDurationSecs = 0
		hub.CallerClient = client
		hub.HubMutex.Unlock()

		go func() {
			// incoming caller will get removed if there is no peerConnect after 11 sec
			// (it can take up to 6-8 seconds in some cases for a devices to get fully out of deep sleep)
			delaySecs := 11
			time.Sleep(time.Duration(delaySecs) * time.Second)

			hub.HubMutex.RLock()
			if hub.CalleeClient!=nil && !hub.CalleeClient.isConnectedToPeer.Get() {
				// only log NO PEERCON if CallerClient.callerOfferForwarded was set (if "callerOffer" was sent)
				if hub.CallerClient!=nil && hub.CallerClient.callerOfferForwarded.Get() {
					// TODO: after 10s, how do we know hub.CallerClient is the same as 10s ago?
					fmt.Printf("%s (%s) NO PEERCON📵 %ds %s <- %s (%s) ua=%s\n",
						client.connType, client.calleeID, delaySecs, hub.CalleeClient.RemoteAddr, 
						hub.CallerClient.RemoteAddr, hub.CallerClient.callerID, hub.CallerClient.userAgent)

					// NOTE: msg MUST NOT contain apostroph (') characters
					msg :=  "Unable to establish a direct P2P connection. "+
					  "This is likely a WebRTC related issue with your browser/WebView, "+
					  "or the browser/WebView on the other side. "+
					  "It could also be a firewall issue. "+
					  "On Android, run <a href=\"https://timur.mobi/webcall/android/#webview\">WebRTC-Check</a> "+
					  "to test your System WebView."
					hub.CallerClient.Write([]byte("status|"+msg))
					hub.CalleeClient.Write([]byte("status|"+msg))
					hub.HubMutex.RUnlock()

					hub.HubMutex.Lock()
					hub.CallerClient = nil
					hub.HubMutex.Unlock()

					// clear CallerIpInHubMap
					err := StoreCallerIpInHubMap(client.globalCalleeID, "", false)
					if err!=nil {
						// err "key not found": callee has already signed off - can be ignored
						if strings.Index(err.Error(),"key not found")<0 {
							fmt.Printf("# %s (%s) NO PEERCON clear callerIpInHub err=%v\n",
								client.connType, client.calleeID, err)
						}
					}
				} else {
					hub.HubMutex.RUnlock()
				}
			} else {
				hub.HubMutex.RUnlock()
			}
		}()

	} else {
		// can be ignored
		//fmt.Printf("# %s (%s/%s) CallerClient already set [%s] %s ws=%d\n",
		//	client.connType, client.calleeID, client.globalCalleeID, hub.CallerClient.RemoteAddr,
		//	client.RemoteAddr, wsClientID64)
	}
}

func (c *WsClient) receiveProcess(message []byte, cliWsConn *websocket.Conn) {
	// check message integrity: cmd's can not be longer than 32 chars
	checkLen := 32
	if len(message) < checkLen {
		checkLen = len(message)
	}
	idxPipe := bytes.Index(message[:checkLen], []byte("|"))
	if idxPipe<0 {
		// invalid -> ignore
		//fmt.Printf("# serveWs receive no pipe char found; abort; checkLen=%d (%s)\n",
		//	checkLen,string(message[:checkLen]))
		return
	}
	tok := strings.Split(string(message),"|")
	if len(tok)!=2 {
		// invalid -> ignore
		fmt.Printf("# serveWs receive len(tok)=%d is !=2; abort; checkLen=%d idxPipe=%d (%s)\n",
			len(tok), checkLen, idxPipe, string(message[:checkLen]))
		return
	}

	//fmt.Printf("_ %s (%s) receive isCallee=%v %s %s\n",
	//	c.connType, c.calleeID, c.isCallee, c.RemoteAddr, cliWsConn.RemoteAddr().String())

	cmd := tok[0]
	payload := tok[1]
	if cmd=="init" {
		if !c.isCallee {
			// only the callee can send "init|"
			fmt.Printf("# %s (%s) deny init is not Callee %s\n", c.connType, c.calleeID, c.RemoteAddr)
			c.Write([]byte("cancel|busy"))
			return
		}

		if c.calleeInitReceived.Get() {
			// only the 1st callee "init|" is accepted
			// don't need to log this
			//fmt.Printf("# %s (%s) deny 2nd callee init %s\n", c.connType, c.calleeID, c.RemoteAddr)
			return
		}

		if c.hub==nil {
			fmt.Printf("# %s (%s) deny init c.hub==nil %s\n", c.connType, c.calleeID, c.RemoteAddr)
			return
		}

		c.hub.HubMutex.Lock()
		c.hub.CallerClient = nil
		c.hub.HubMutex.Unlock()

		c.calleeInitReceived.Set(true)
		c.hub.CalleeLogin.Set(true)
		c.pickupSent.Set(false)
		// doUnregister() will call setDeadline(0) and processTimeValues() if this is false; then set it true
		c.clearOnCloseDone = false // TODO make it atomic?
		c.callerTextMsg = ""

		if logWantedFor("attach") {
			fmt.Printf("%s (%s) callee init ws=%d %s ver=%s\n",
				c.connType, c.calleeID, c.hub.WsClientID, c.RemoteAddr, c.clientVersion)
		}

		// TODO should we clear callerIpInHubMap via StoreCallerIpInHubMap(,"") just to be sure?
		//StoreCallerIpInHubMap(c.globalCalleeID, "", false)

		// deliver the callee client version number
		readConfigLock.RLock()
		calleeClientVersionTmp := calleeClientVersion
		readConfigLock.RUnlock()
		if c.Write([]byte("sessionId|"+calleeClientVersionTmp)) != nil {
			return
		}

		if !strings.HasPrefix(c.calleeID,"answie") && !strings.HasPrefix(c.calleeID,"talkback") {
			if clientUpdateBelowVersion!="" && c.clientVersion < clientUpdateBelowVersion {
				//fmt.Printf("%s (%s) ver=%s\n",c.connType,c.calleeID,c.clientVersion)
				// NOTE: msg MUST NOT contain apostroph (') characters
				msg := "This version of WebCall for Android has a technical problem. "+
						"Support will be phased out soon. "+
						"Please upgrade to <a href=\"https://timur.mobi/webcall/update/\">v1.0 or newer.</a>"
				if logWantedFor("login") {
					fmt.Printf("%s (%s) send status|%s\n",c.connType,c.calleeID,msg)
				}
				c.Write([]byte("status|"+msg))
				return
			}

			// send list of waitingCaller and missedCalls to callee client
			var waitingCallerSlice []CallerInfo
			// err can be ignored
			kvCalls.Get(dbWaitingCaller,c.calleeID,&waitingCallerSlice)
			// before we send waitingCallerSlice
			// we remove all entries that are older than 10min
			countOutdated:=0
			for idx := range waitingCallerSlice {
				//fmt.Printf("%s (idx=%d of %d)\n", c.connType,idx,len(waitingCallerSlice))
				if idx >= len(waitingCallerSlice) {
					break
				}
				if time.Now().Unix() - waitingCallerSlice[idx].CallTime > 10*60 {
					// remove outdated caller from waitingCallerSlice
					waitingCallerSlice = append(waitingCallerSlice[:idx],
						waitingCallerSlice[idx+1:]...)
					countOutdated++
				}
			}
			var err error
			if countOutdated>0 {
				fmt.Printf("%s (%s) deleted %d outdated from waitingCallerSlice\n",
					c.connType, c.calleeID, countOutdated)
				err = kvCalls.Put(dbWaitingCaller, c.calleeID, waitingCallerSlice, true) // skipConfirm
				if err!=nil {
					fmt.Printf("# %s (%s) failed to store dbWaitingCaller\n",c.connType,c.calleeID)
				}
			}

			var missedCallsSlice []CallerInfo
			// err can be ignored
			kvCalls.Get(dbMissedCalls,c.calleeID,&missedCallsSlice)

			if len(waitingCallerSlice)>0 || len(missedCallsSlice)>0 {
				if logWantedFor("waitingCaller") {
					fmt.Printf("%s (%s) waitingCaller=%d missedCalls=%d\n",c.connType,c.calleeID,
						len(waitingCallerSlice),len(missedCallsSlice))
				}
				// -> httpServer c.Write()
				waitingCallerToCallee(c.calleeID, waitingCallerSlice, missedCallsSlice, c)
			}
		}
		//if logWantedFor("login") {
		//	fmt.Printf("%s (%s) callee init done\n", c.connType, c.calleeID)
		//}
		return
	}

	if cmd=="dummy" {
		fmt.Printf("%s (%s) dummy %s ip=%s ua=%s\n",
			c.connType, c.calleeID, payload, c.RemoteAddr, c.userAgent)
		return
	}

	if cmd=="msg" {
		// sent by caller on hangup without mediaconnect
		cleanMsg := strings.Replace(payload, "\n", " ", -1)
		cleanMsg = strings.Replace(cleanMsg, "\r", " ", -1)
		cleanMsg = strings.TrimSpace(cleanMsg)
		if c.hub==nil {
			fmt.Printf("# %s (%s) msg='%s' c.hub==nil callee=%v ip=%s ua=%s\n",
				c.connType, c.calleeID, cleanMsg, c.isCallee, c.RemoteAddr, c.userAgent)
			return
		}
		c.hub.HubMutex.Lock()
		if c.hub.CalleeClient==nil {
			fmt.Printf("# %s (%s) msg='%s' c.hub.CalleeClient==nil callee=%v ip=%s ua=%s\n",
				c.connType, c.calleeID, cleanMsg, c.isCallee, c.RemoteAddr, c.userAgent)
		} else {
			fmt.Printf("%s (%s) msg='%s' callee=%v ip=%s ua=%s\n",
				c.connType, c.calleeID, cleanMsg, c.isCallee, c.RemoteAddr, c.userAgent)
			c.hub.CalleeClient.callerTextMsg = cleanMsg;
		}
		c.hub.HubMutex.Unlock()
		return
	}

	if cmd=="missedcall" {
		// sent by caller on hangup without mediaconnect
		fmt.Printf("%s (%s) missedcall='%s' callee=%v ip=%s ua=%s\n",
			c.connType, c.calleeID, payload, c.isCallee, c.RemoteAddr, c.userAgent)
		//c.hub.CalleeClient.callerTextMsg = payload;
		missedCall(payload, c.RemoteAddr, "cmd=missedcall")
		return
	}

	if cmd=="callerOffer" {
		// caller starting a call - payload is JSON.stringify(localDescription)
		if c.callerOfferForwarded.Get() {
			// prevent double callerOffer
			//fmt.Printf("# %s (%s) CALL from %s was already forwarded\n",
			//	c.connType, c.calleeID, c.RemoteAddr)
			return
		}

		//fmt.Printf("%s (%s) callerOffer... %s\n", c.connType, c.calleeID, c.RemoteAddr)

		c.hub.HubMutex.RLock()
		if c.hub.CalleeClient==nil {
			c.hub.HubMutex.RUnlock()
			fmt.Printf("# %s (%s) CALL from %s but hub.CalleeClient==nil\n",
				c.connType, c.calleeID, c.RemoteAddr)
			return
		}
		if c.hub.CallerClient==nil {
			fmt.Printf("# %s (%s) CALL☎️ but hub.CallerClient==nil\n",
				c.connType, c.calleeID)
			c.hub.HubMutex.RUnlock()
			return
		}

		fmt.Printf("%s (%s) CALL☎️ %s <- %s (%s) ua=%s\n",
			c.connType, c.calleeID, c.hub.CalleeClient.RemoteAddr,
			c.hub.CallerClient.RemoteAddr, c.hub.CallerClient.callerID, c.hub.CallerClient.userAgent)

		// forward the callerOffer message to the callee client
		if c.hub.CalleeClient.Write(message) != nil {
			c.hub.HubMutex.RUnlock()
			return
		}
		c.callerOfferForwarded.Set(true)

		if c.hub.CallerClient.callerID!="" || c.hub.CallerClient.callerName!="" {
			// send this directly to the callee: see callee.js if(cmd=="callerInfo")
			sendCmd := "callerInfo|"+c.hub.CallerClient.callerID+":"+c.hub.CallerClient.callerName
			if c.hub.CalleeClient.Write([]byte(sendCmd)) != nil {
				c.hub.HubMutex.RUnlock()
				return
			}
		}

		// exchange useragent's
		if c.hub.CallerClient.Write([]byte("ua|"+c.hub.CalleeClient.userAgent)) != nil {
			c.hub.HubMutex.RUnlock()
			return
		}
		if c.hub.CalleeClient.Write([]byte("ua|"+c.hub.CallerClient.userAgent)) != nil {
			c.hub.HubMutex.RUnlock()
			return
		}
		c.hub.HubMutex.RUnlock()

		if c.hub.maxRingSecs>0 {
			// if callee does NOT pickup the call after c.hub.maxRingSecs, callee will be disconnected
			c.hub.setDeadline(c.hub.maxRingSecs,"serveWs ringsecs")
		}
		// this is needed for turn AuthHandler: store caller RemoteAddr
		err := StoreCallerIpInHubMap(c.globalCalleeID, c.RemoteAddr, false)
		if err!=nil {
			fmt.Printf("# %s (%s) callerOffer StoreCallerIp %s err=%v\n",
				c.connType, c.globalCalleeID, c.RemoteAddr, err)
		} else {
			if logWantedFor("wscall") {
				fmt.Printf("%s (%s) callerOffer StoreCallerIp %s\n",
					c.connType, c.globalCalleeID, c.RemoteAddr)
			}
		}
		return
	}

	if cmd=="rtcConnect" {
		return
	}

	if cmd=="cancel" {
		// not sure which client is sending this
		//fmt.Printf("%s (%s) cmd=cancel payload=%s %s\n",c.connType,c.calleeID,payload,c.RemoteAddr)
		if c.hub==nil {
			fmt.Printf("# %s cmd=cancel but c.hub==nil %s (%s)\n",c.connType,c.RemoteAddr,payload)
			return
		}
		c.hub.HubMutex.RLock()
		if c.hub.CalleeClient==nil {
			c.hub.HubMutex.RUnlock()
			// we receive a "cmd=cancel|" (from the caller?) but the callee is logged out
			//fmt.Printf("# %s cmd=cancel but c.hub.CalleeClient==nil %s (%s)\n",c.connType,c.RemoteAddr,payload)
			c.Close("callee already closed")
			return
		}

		if c.hub.CalleeClient.isConnectedToPeer.Get() {
			// unlock - don't call peerConHasEnded with lock
			c.hub.HubMutex.RUnlock()
			// only execute cancel, if callee is peer-connected
			if c.isCallee {
				fmt.Printf("%s (%s) DISCON from callee %s '%s'\n", c.connType, c.calleeID, c.RemoteAddr, payload)
			} else {
				fmt.Printf("%s (%s) DISCON from caller %s '%s'\n", c.connType, c.calleeID, c.RemoteAddr, payload)
			}
			// tell callee to disconnect
			c.hub.CalleeClient.peerConHasEnded("cancel")
		} else {
			c.hub.HubMutex.RUnlock()
			// ignore, already disconnected
			//fmt.Printf("%s (%s) ignore cmd=cancel connected=%v c.isCallee=%v %s '%s'\n",
			//	c.connType, c.calleeID, c.hub.CalleeClient.isConnectedToPeer.Get(),
			//	c.isCallee, c.RemoteAddr, payload)
		}
		return
	}

	if cmd=="calleeHidden" {
		//fmt.Printf("%s cmd=calleeHidden from %s (%s)\n",c.connType,c.RemoteAddr,payload)
		c.hub.HubMutex.Lock()
		if(payload=="true") {
			c.hub.IsCalleeHidden = true
		} else {
			c.hub.IsCalleeHidden = false
		}
		c.hub.IsUnHiddenForCallerAddr = ""
		calleeHidden := c.hub.IsCalleeHidden
		c.hub.HubMutex.Unlock()

		// forward state of c.isHiddenCallee to globalHubMap
		err := SetCalleeHiddenState(c.calleeID, calleeHidden)
		if err != nil {
			fmt.Printf("# serveWs (%s) SetCalleeHiddenState %v err=%v\n", c.calleeID, calleeHidden, err)
		}

		// read dbUser for IsCalleeHidden flag
		// store dbUser after set/clear IsCalleeHidden in dbUser.Int2&1
		userKey := c.calleeID + "_" + strconv.FormatInt(int64(c.hub.registrationStartTime),10)
		var dbUser DbUser
		err = kvMain.Get(dbUserBucket, userKey, &dbUser)
		if err!=nil {
			fmt.Printf("# serveWs (%s) cmd=calleeHidden db=%s bucket=%s getX key=%v err=%v\n",
				c.calleeID, dbMainName, dbUserBucket, userKey, err)
		} else {
			if calleeHidden {
				dbUser.Int2 |= 1
			} else {
				dbUser.Int2 &= ^1
			}
			fmt.Printf("%s (%s) set hidden=%v %d %s %s\n", c.connType, c.calleeID,
				calleeHidden, dbUser.Int2, userKey, c.RemoteAddr)
			err := kvMain.Put(dbUserBucket, userKey, dbUser, true) // skipConfirm
			if err!=nil {
				fmt.Printf("# serveWs (%s) calleeHidden db=%s bucket=%s put key=%v %s err=%v\n",
					c.calleeID, dbMainName, dbUserBucket, userKey, c.RemoteAddr, err)
			} else {
				//fmt.Printf("%s calleeHidden db=%s bucket=%s put key=%v OK\n",
				//	c.connType, dbMainName, dbUserBucket, userKey)
				/*
				// this was used for verification only
				var dbUser2 DbUser
				err := kvMain.Get(dbUserBucket, userKey, &dbUser2)
				if err!=nil {
					fmt.Printf("# serveWs calleeHidden verify db=%s bucket=%s getX key=%v err=%v\n",
						dbMainName, dbUserBucket, userKey, err)
				} else {
					fmt.Printf("serveWs calleeHidden verify userKey=%v isHiddenCallee=%v (%d)\n",
						userKey, dbUser2.Int2&1!=0, dbUser2.Int2)
				}
				*/
			}
		}
		return
	}

	if cmd=="dialsoundsmuted" {
		dialSoundsMuted := false
		if(payload=="true") {
			dialSoundsMuted = true
		}

		// read dbUser for dialSoundsMuted flag
		userKey := c.calleeID + "_" + strconv.FormatInt(int64(c.hub.registrationStartTime),10)
		var dbUser DbUser
		err := kvMain.Get(dbUserBucket, userKey, &dbUser)
		if err!=nil {
			fmt.Printf("# serveWs (%s) cmd=dialsounds db=%s bucket=%s getX key=%v err=%v\n",
				c.calleeID, dbMainName, dbUserBucket, userKey, err)
		} else {
			// store dbUser after set/clear dialSoundsMuted in dbUser.Int2&4
			if dialSoundsMuted {
				dbUser.Int2 |= 4
			} else {
				dbUser.Int2 &= ^4
			}
			fmt.Printf("%s (%s) set dialSoundsMuted=%v %d %s %s\n", c.connType, c.calleeID,
				dialSoundsMuted, dbUser.Int2, userKey, c.RemoteAddr)
			err := kvMain.Put(dbUserBucket, userKey, dbUser, true) // skipConfirm
			if err!=nil {
				fmt.Printf("# serveWs (%s) dialSoundsMuted db=%s bucket=%s put key=%v %s err=%v\n",
					c.calleeID, dbMainName, dbUserBucket, userKey, c.RemoteAddr, err)
			}
		}
		return
	}

	if cmd=="pickupWaitingCaller" {
		// for callee only
		// payload = ip:port
		callerAddrPort := payload
		fmt.Printf("%s pickupWaitingCaller from %s (%s)\n", c.connType, c.RemoteAddr, callerAddrPort)
		// this will end the frozen xhr call by the caller in httpNotifyCallee.go (see: case <-c)
		waitingCallerChanMap[callerAddrPort] <- 1
		return
	}

	if cmd=="deleteMissedCall" {
		// for callee only: payload = ip:port:callTime
		callerAddrPortPlusCallTime := payload
		//fmt.Printf("%s deleteMissedCall from %s callee=%s (payload=%s)\n",
		//	c.connType, c.RemoteAddr, c.calleeID, callerAddrPortPlusCallTime)

		// remove this call from dbMissedCalls for c.calleeID
		// first: load dbMissedCalls for c.calleeID
		var missedCallsSlice []CallerInfo
//		c.hub.HubMutex.RLock()
		userKey := c.calleeID + "_" + strconv.FormatInt(int64(c.hub.registrationStartTime),10)
//		c.hub.HubMutex.RUnlock()
		var dbUser DbUser
		err := kvMain.Get(dbUserBucket, userKey, &dbUser)
		if err!=nil {
			fmt.Printf("# %s (%s) failed to get dbUser\n",c.connType,c.calleeID)
		} else if(dbUser.StoreMissedCalls) {
			err = kvCalls.Get(dbMissedCalls,c.calleeID,&missedCallsSlice)
			if err!=nil {
				fmt.Printf("# serveWs deleteMissedCall (%s) failed to read dbMissedCalls\n",c.calleeID)
			}
		}
		if len(missedCallsSlice)>0 {
			//fmt.Printf("serveWs deleteMissedCall (%s) found %d entries\n",
			//	c.calleeID, len(missedCallsSlice))
			// search for callerIP:port + CallTime == callerAddrPortPlusCallTime
			for idx := range missedCallsSlice {
				//id := fmt.Sprintf("%s_%d",missedCallsSlice[idx].AddrPort,missedCallsSlice[idx].CallTime)
				id := missedCallsSlice[idx].AddrPort + "_" +
					 strconv.FormatInt(int64(missedCallsSlice[idx].CallTime),10)
				//fmt.Printf("deleteMissedCall %s compare (%s==%s)\n", callerAddrPortPlusCallTime, id)
				if id == callerAddrPortPlusCallTime {
					//fmt.Printf("serveWs deleteMissedCall idx=%d\n",idx)
					missedCallsSlice = append(missedCallsSlice[:idx], missedCallsSlice[idx+1:]...)
					// store modified dbMissedCalls for c.calleeID
					err := kvCalls.Put(dbMissedCalls, c.calleeID, missedCallsSlice, false)
					if err!=nil {
						fmt.Printf("# serveWs deleteMissedCall (%s) fail store dbMissedCalls\n", c.calleeID)
					}
					// send modified missedCallsSlice to callee
					json, err := json.Marshal(missedCallsSlice)
					if err != nil {
						fmt.Printf("# serveWs deleteMissedCall (%s) failed json.Marshal\n", c.calleeID)
					} else {
						//fmt.Printf("deleteMissedCall send missedCallsSlice %s\n", c.calleeID)
						c.hub.HubMutex.RLock()
						if c.hub.CalleeClient!=nil {
							c.hub.CalleeClient.Write([]byte("missedCalls|"+string(json)))
						}
						c.hub.HubMutex.RUnlock()
					}
					break
				}
			}
		}
		return
	}

	if cmd=="pickup" {
		// this is sent by the callee client
		if !c.isConnectedToPeer.Get() {
			if logWantedFor("login") {
				fmt.Printf("# %s (%s) pickup ignored no peerConnect %s\n",
					c.connType, c.calleeID, c.RemoteAddr)
			}
			return
		}
		if c.pickupSent.Get() {
			// prevent sending 'pickup' twice
			//fmt.Printf("# %s (%s) pickup ignored already sent %s\n",
			//	c.connType, c.calleeID, c.RemoteAddr)
			return
		}

		c.hub.HubMutex.Lock()
		c.hub.lastCallStartTime = time.Now().Unix()
		c.hub.HubMutex.Unlock()
		if logWantedFor("hub") {
			fmt.Printf("%s (%s) pickup online=%v peerCon=%v starttime=%d\n",
				c.connType, c.calleeID, c.isOnline.Get(), c.isConnectedToPeer.Get(), c.hub.lastCallStartTime)
		}
		c.hub.HubMutex.RLock()
		if c.hub.CallerClient!=nil {
			// deliver "pickup" to the caller
			if logWantedFor("wscall") {
				fmt.Printf("%s (%s) forward pickup to caller (%s)\n", c.connType, c.calleeID, message)
			}
			c.hub.CallerClient.Write(message)
			c.pickupSent.Set(true)
		}
		c.hub.HubMutex.RUnlock()
		c.hub.setDeadline(0,"pickup")
		return
	}

	if cmd=="heartbeat" {
		// ignore: clients may send this to check the connection to the server
		return
	}

	if cmd=="check" {
		// clients may send this to check communication with the server
		// server sends payload back to client
		c.Write([]byte("confirm|"+payload))
		return
	}

	if cmd=="log" {
		// TODO make extra sure payload is not malformed
		if c==nil {
			fmt.Printf("# peer c==nil\n")
			return
		}
		if c.hub==nil {
			fmt.Printf("# %s (%s) peer c.hub==nil ver=%s\n", c.connType, c.calleeID, c.clientVersion)
			return
		}

		c.hub.HubMutex.RLock()
		if c.hub.CallerClient==nil {
			c.hub.HubMutex.RUnlock()
			// # serveWss (id) peer 'callee Connected unknw/unknw'
			// this happens when caller disconnects immediately
			// or when caller is late and callee has already peer-disconnected
			fmt.Printf("# %s (%s) peer %s isCallee=%v c.hub.CallerClient==nil ver=%s\n",
				c.connType, c.calleeID, payload, c.isCallee, c.clientVersion)
			// TODO this may come to late?
			StoreCallerIpInHubMap(c.globalCalleeID, "", false)
			return
		}
		if c.hub.CalleeClient==nil {
			c.hub.HubMutex.RUnlock()
			fmt.Printf("# %s (%s) peer %s c.hub.CalleeClient==nil ver=%s\n",
				c.connType, c.calleeID, payload, c.clientVersion)
			// TODO this makes no sense if hub.CalleeClient==nil ?
			//StoreCallerIpInHubMap(c.globalCalleeID, "", false)
			return
		}

		// payload = "callee Connected p2p/p2p"
		tok := strings.Split(payload, " ")

		// payload = "callee Incoming p2p/p2p" or "callee Connected p2p/p2p"
		// "%s (%s) peer callee Incoming p2p/p2p" or "%s (%s) peer callee Connected p2p/p2p"
		// note: "callee Connected p2p/p2p" can happen multiple times
		constate := ""
		constateShort := "-"
		if len(tok)>=2 {
			constate = strings.TrimSpace(tok[1])
			if constate=="Incoming"  { constateShort = "RING" }
			if constate=="Connected" { constateShort = "CONN" }
			if constate=="ConForce"  { constateShort = "CONF" }
		}
		if tok[0]=="callee" {
			fmt.Printf("%s (%s) PEER %s %s %s %s <- %s (%s)\n",
				c.connType, c.calleeID, tok[0], constateShort, tok[2], c.hub.CalleeClient.RemoteAddrNoPort,
				c.hub.CallerClient.RemoteAddrNoPort, c.hub.CallerClient.callerID)
		} else {
			fmt.Printf("%s (%s) PEER %s %s %s %s <- %s (%s)\n",
				c.connType, c.calleeID, tok[0], constateShort, tok[2], c.hub.CalleeClient.RemoteAddrNoPort,
				c.hub.CallerClient.RemoteAddrNoPort, c.hub.CallerClient.callerID)
		}

		if len(tok)>=2 && (constate=="Incoming" || constate=="Connected" || constate=="ConForce") {
			//fmt.Printf("%s (%s) set ConnectedToPeer\n", c.connType, c.calleeID)
			// note: we only make sure that callee has this always set
			// if we only get "Incoming" for callee, then isConnectedToPeer will not be set for CallerClient
			c.isConnectedToPeer.Set(true) // this is peer-connect, not full media-connect
			if !c.isCallee {
				// when the caller sends "log", the callee also becomes peerConnected
				c.hub.CalleeClient.isConnectedToPeer.Set(true)
			}

			c.hub.LocalP2p = false
			c.hub.RemoteP2p = false
			if len(tok)>=3 {
				tok2string := strings.TrimSpace(tok[2])
				tok2 := strings.Split(tok2string, "/")
				if len(tok2)>=2 {
					//fmt.Printf("%s tok2[0]=%s tok2[1]=%s\n", c.connType, tok2[0], tok2[1])
					if tok2[0]=="p2p" {
						c.hub.LocalP2p = true
					}
					if tok2[1]=="p2p" {
						c.hub.RemoteP2p = true
					}
				} else {
					fmt.Printf("# %s tok2string=%s has no slash\n", c.connType, tok2string)
				}
			} else {
				fmt.Printf("# %s len(tok)<3\n", c.connType)
			}

			if constate=="Connected" || constate=="ConForce" {
				if c.isCallee {
					// callee reports: peer connected (this may happen multiple times)
					if !c.isMediaConnectedToPeer.Get() {
						// only on 1st callee peer connect: set peer media connect for both sides
						c.isMediaConnectedToPeer.Set(true)
						c.hub.CallerClient.isMediaConnectedToPeer.Set(true)

						if maxClientRequestsPer30min>0 {
							clientRequestsMutex.Lock()
							//clientRequestsMap[c.RemoteAddrNoPort] = nil
							//clientRequestsMap[c.hub.CallerClient.RemoteAddrNoPort] = nil
							delete(clientRequestsMap,c.RemoteAddrNoPort)
							delete(clientRequestsMap,c.hub.CallerClient.RemoteAddrNoPort)
							clientRequestsMutex.Unlock()
						}
						// TODO also reset calleeLoginMap?

						if c.hub.maxTalkSecsIfNoP2p>0 && (!c.hub.LocalP2p || !c.hub.RemoteP2p) {
							// relayed con: set deadline maxTalkSecsIfNoP2p
							//if logWantedFor("deadline") {
							//	fmt.Printf("%s (%s) setDeadline maxTalkSecsIfNoP2p %d %v %v\n", c.connType,
							//		c.calleeID, c.hub.maxTalkSecsIfNoP2p, c.hub.LocalP2p, c.hub.RemoteP2p)
							//}
							c.hub.HubMutex.RUnlock()
							c.hub.setDeadline(c.hub.maxTalkSecsIfNoP2p,"peer con")
							c.hub.HubMutex.RLock()

							// deliver max talktime to both clients
							c.hub.doBroadcast(
								[]byte("sessionDuration|"+strconv.FormatInt(int64(c.hub.maxTalkSecsIfNoP2p),10)))
						}
					}
				} else {
					// caller reports: peer connected
					if constate=="ConForce" {
						// test-caller sends this msg to callee, test-clients do not really connect p2p
						c.hub.CalleeClient.Write([]byte("callerConnect|"))
					} else if constate=="Connected" {
						// caller is reporting peerCon: both peers are now directly connected
						// now force-disconnect the caller
						readConfigLock.RLock()
						myDisconCalleeOnPeerConnected := disconCalleeOnPeerConnected
						myDisconCallerOnPeerConnected := disconCallerOnPeerConnected
						readConfigLock.RUnlock()
						if myDisconCalleeOnPeerConnected || myDisconCallerOnPeerConnected {
							time.Sleep(20 * time.Millisecond)
						}
						if myDisconCalleeOnPeerConnected {
							// this is currently never done
							fmt.Printf("%s peer callee disconnect %s %s\n",
								c.connType, c.calleeID, c.RemoteAddr)
							c.hub.CalleeClient.Close("disconCalleeOnPeerConnected")
						}
						if myDisconCallerOnPeerConnected {
							// this is currently always done
							if c.hub.CallerClient != nil {
								//fmt.Printf("%s (%s) peer caller disconnect %s\n",
								//	c.connType, c.calleeID, c.RemoteAddr)
								c.hub.CallerClient.Close("disconCallerOnPeerConnected")
							}
						}
					}
				}
			}
		}
		c.hub.HubMutex.RUnlock()
		return
	}

/* TODO ???
	if !c.isCallee && c.hub!=nil {
		// client is caller
		c.hub.HubMutex.RLock()
		if !c.hub.CalleeClient.isOnline.Get() {
			c.hub.HubMutex.RUnlock()
			// but there is no callee
			fmt.Printf("# %s client %s without callee not allowed (%s)\n",
				c.connType, c.RemoteAddr, cmd)
			c.Write([]byte("cancel|busy"))
			return
		}
		if c.hub.CallerClient!=nil && c.hub.CallerClient!=c {
			c.hub.HubMutex.RUnlock()
			// but there is already another caller-client
			fmt.Printf("# %s client %s is 2nd client not allowed\n",
				c.connType, c.RemoteAddr)
			c.Write([]byte("cancel|busy"))
			return
		}
		c.hub.HubMutex.RUnlock()
	}
*/

	if len(payload)>0 {
		// forward cmd/payload to other client
		if c.hub!=nil {
			if logWantedFor("wsreceive") {
				fmt.Printf("%s recv/fw %s|%s iscallee=%v %s\n",
					c.connType, cmd, payload, c.isCallee, c.RemoteAddr)
			} else {
				//fmt.Printf("%s recv/fw %s iscallee=%v %s\n",
				//	c.connType, cmd, c.isCallee, c.RemoteAddr)
			}
			c.hub.HubMutex.RLock()
			if c.isCallee {
				if c.hub.CallerClient!=nil {
					c.hub.CallerClient.Write(message)
				}
			} else {
				if c.hub.CalleeClient!=nil {
					c.hub.CalleeClient.Write(message)
				}
			}
			c.hub.HubMutex.RUnlock()
		}
	} else {
		//fmt.Printf("%s %s with no payload\n",c.connType,cmd)
	}
}

func (c *WsClient) Write(b []byte) error {
	max := len(b); if max>22 { max = 22 }
	if !c.isOnline.Get() {
		//fmt.Printf("# %s Write (%s) to %s callee=%v peerCon=%v NOT ONLINE\n",
		//	c.connType, b[:max], c.calleeID, c.isCallee, c.isConnectedToPeer.Get())
		return ErrWriteNotConnected
	}
	if logWantedFor("wswrite") {
		fmt.Printf("%s Write (%s) to %s callee=%v peerCon=%v\n",
			c.connType, b[:max], c.calleeID, c.isCallee, c.isConnectedToPeer.Get())
	}

	c.wsConn.WriteMessage(websocket.TextMessage, b)
	return nil
}

func (c *WsClient) peerConHasEnded(cause string) {
	// the peerConnection has ended, either bc one side has sent cmd "cancel"
	// or bc callee has unregistered
	if c==nil {
		fmt.Printf("# peerConHasEnded but c==nil\n")
		return
	}

	if c.hub==nil {
		fmt.Printf("# %s (%s) peerConHasEnded callee=%v con=%v media=%v c.hub==nil (%s)\n",
			c.connType, c.calleeID, c.isCallee,
			c.isConnectedToPeer.Get(), c.isMediaConnectedToPeer.Get(), cause)
		return
	}

	c.hub.setDeadline(0,cause)	// may call peerConHasEnded()

	if c.hub.lastCallStartTime>0 {
		c.hub.processTimeValues("peerConHasEnded") // will set c.hub.CallDurationSecs
		c.hub.lastCallStartTime = 0
	}

	if !c.isCallee {
		// this does not happen anymore
		fmt.Printf("# %s (%s) peerConHasEnded (ignore isCaller) con=%v media=%v (%s)\n",
			c.connType, c.calleeID, //peerType,
			c.isConnectedToPeer.Get(), c.isMediaConnectedToPeer.Get(), cause)
		return
	}

	// prepare for next session
	c.calleeInitReceived.Set(false)

	if c.isConnectedToPeer.Get() {
		// we are disconnection a peer connect
		if logWantedFor("attach") {
			fmt.Printf("%s (%s) peerConHasEnded con=%v media=%v (%s)\n",
				c.connType, c.calleeID,
				c.isConnectedToPeer.Get(), c.isMediaConnectedToPeer.Get(), cause)
		}

		localPeerCon := "?"
		remotePeerCon := "?"
		if c.hub!=nil {
			localPeerCon = "p2p"
			if !c.hub.LocalP2p { localPeerCon = "relay" }
			remotePeerCon = "p2p"
			if !c.hub.RemoteP2p { remotePeerCon = "relay" }
		}

		c.isConnectedToPeer.Set(false)
		c.isMediaConnectedToPeer.Set(false)
		// now clear these two flags also on the other side
//		if c.isCallee {
			c.hub.HubMutex.RLock()
			if c.hub.CallerClient!=nil {
				c.hub.CallerClient.isConnectedToPeer.Set(false)
				c.hub.CallerClient.isMediaConnectedToPeer.Set(false)
			}
			c.hub.HubMutex.RUnlock()
//		} else {
//			// this does not happen anymore
//			c.hub.HubMutex.RLock()
//			if c.hub.CalleeClient!=nil {
//				c.hub.CalleeClient.isConnectedToPeer.Set(false)
//				c.hub.CalleeClient.isMediaConnectedToPeer.Set(false)
//			}
//			c.hub.HubMutex.RUnlock()
//		}

		calleeRemoteAddr := ""
		callerRemoteAddr := ""
		callerID := ""
		callerName := ""
		c.hub.HubMutex.RLock()
		if c.hub.CalleeClient!=nil {
			calleeRemoteAddr = c.hub.CalleeClient.RemoteAddrNoPort
		}
		if c.hub.CallerClient!=nil  {
			callerRemoteAddr = c.hub.CallerClient.RemoteAddrNoPort
			callerID = c.hub.CallerClient.callerID
			callerName = c.hub.CallerClient.callerName

			// clear recentTurnCalleeIps[ipNoPort] entry (if this was a relay session)
			recentTurnCalleeIpMutex.Lock()
			delete(recentTurnCalleeIps,c.hub.CallerClient.RemoteAddrNoPort)
			recentTurnCalleeIpMutex.Unlock()
		}
		c.hub.HubMutex.RUnlock()
		fmt.Printf("%s (%s) PEER DISCON📴 %ds %s/%s %s <- %s (%s) %s\n",
			c.connType, c.calleeID, //peerType,
			c.hub.CallDurationSecs, localPeerCon, remotePeerCon,
			calleeRemoteAddr, callerRemoteAddr, callerID, cause)

		// add an entry to missed calls, but only if hub.CallDurationSecs==0
		// TODO is it a missed call if callee denies the call? (currently yes)
		if c.hub.CallDurationSecs<=0 {
			//if logWantedFor("missedcall") {
			//	fmt.Printf("%s (%s) store missedCall %s ...\n", c.connType, c.calleeID, c.RemoteAddr)
			//}
			userKey := c.calleeID + "_" + strconv.FormatInt(int64(c.hub.registrationStartTime),10)
			var dbUser DbUser
			err := kvMain.Get(dbUserBucket, userKey, &dbUser)
			if err!=nil {
				fmt.Printf("# %s (%s) failed to get dbUser err=%v\n",c.connType,c.calleeID,err)
			} else if dbUser.StoreMissedCalls {
				// if caller cancels via hangup button, then this is the only addMissedCall() and contains msgtext
				// if caller exits page, it sends /missedcall with msgtext
				//   but this becomes a double addMissedCall() without msgtext
				//fmt.Printf("%s (%s) store missedCall msg=(%s)\n", c.connType, c.calleeID, c.callerTextMsg)
				addMissedCall(c.calleeID, CallerInfo{callerRemoteAddr, callerName, time.Now().Unix(),
					callerID, c.callerTextMsg}, cause)
			}
		}

		//if logWantedFor("attach") {
		//	fmt.Printf("%s (%s) peerConHasEnded clr CallerIp %s\n", c.connType, c.calleeID, c.globalCalleeID)
		//}
		err := StoreCallerIpInHubMap(c.globalCalleeID, "", false)
		if err!=nil {
			// err "key not found": callee has already signed off - can be ignored
			//if strings.Index(err.Error(),"key not found")<0 {
				fmt.Printf("# %s (%s) peerConHasEnded clr callerIp %s err=%v\n",
					c.connType, c.calleeID, c.globalCalleeID, err)
			//}
		}

		// this will prevent NO PEERCON after hangup or after calls shorter than 10s
		c.hub.HubMutex.Lock()
		c.hub.CallerClient = nil
		c.hub.HubMutex.Unlock()
	}

	//if logWantedFor("attach") {
	//	fmt.Printf("%s (%s) peerConHasEnded %s done (%s)\n", c.connType, c.calleeID, peerType, comment)
	//}
}

func (c *WsClient) Close(reason string) {
	if c.isOnline.Get() {
		if logWantedFor("wsclose") {
			fmt.Printf("wsclient Close %s callee=%v %s\n", c.calleeID, c.isCallee, reason)
		}
		c.wsConn.WriteMessage(websocket.CloseMessage, nil)
		c.wsConn.Close()
	}
}

func (c *WsClient) SendPing(maxWaitMS int) {
	if logWantedFor("sendping") {
		fmt.Printf("sendPing %s %s\n",c.wsConn.RemoteAddr().String(), c.calleeID)
	}

	// set the time for sending the next ping in pingPeriod secs from now
	keepAliveMgr.SetPingDeadline(c.wsConn, pingPeriod, c)

	// we expect a pong (or anything) from the client within max 20 secs from now
	if maxWaitMS<0 {
		maxWaitMS = 20000
	}
	c.wsConn.SetReadDeadline(time.Now().Add(time.Duration(maxWaitMS)*time.Millisecond))

	c.wsConn.WriteMessage(websocket.PingMessage, nil)
	c.pingSent++
}


// KeepAliveMgr done with kind support from lesismal of github.com/lesismal/nbio
// Keeping many idle clients alive: https://github.com/lesismal/nbio/issues/92 
type KeepAliveMgr struct {
	mux       sync.RWMutex
	clients   map[*websocket.Conn]struct{}
}

func NewKeepAliveMgr() *KeepAliveMgr {
	return &KeepAliveMgr{
		clients: make(map[*websocket.Conn]struct{}),
	}
}

type KeepAliveSessionData struct {
	pingSendTime time.Time
	client *WsClient
}

func (kaMgr *KeepAliveMgr) SetPingDeadline(wsConn *websocket.Conn, secs int, client *WsClient) {
	// set the absolute time for sending the next ping
	wsConn.SetSession(&KeepAliveSessionData{
		time.Now().Add(time.Duration(secs)*time.Second), client})
}

func (kaMgr *KeepAliveMgr) Add(c *websocket.Conn) {
	kaMgr.mux.Lock()
	defer kaMgr.mux.Unlock()
	kaMgr.clients[c] = struct{}{}
}

func (kaMgr *KeepAliveMgr) Delete(c *websocket.Conn) {
	kaMgr.mux.Lock()
	defer kaMgr.mux.Unlock()
	delete(kaMgr.clients,c)
}

func (kaMgr *KeepAliveMgr) Run() {
	ticker := time.NewTicker(2*time.Second)
	defer ticker.Stop()
	for {
		<-ticker.C
		if shutdownStarted.Get() {
			break
		}
		kaMgr.mux.RLock()
		myClients := make([]*websocket.Conn, len(kaMgr.clients))
		i:=0
		for wsConn := range kaMgr.clients {
			myClients[i] = wsConn
			i++
		}
		kaMgr.mux.RUnlock()

		var nPing int64 = 0
		timeNow := time.Now()
		for _,wsConn := range myClients {
			keepAliveSessionData := wsConn.Session().(*KeepAliveSessionData)
			if keepAliveSessionData!=nil {
				if timeNow.After(keepAliveSessionData.pingSendTime) {
					keepAliveSessionData.client.SendPing(-1)
					nPing++
				}
			}
		}
		atomic.AddInt64(&pingSentCounter, nPing)
	}
}

