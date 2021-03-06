// gorewind is an event store server written in Python that talks ZeroMQ.
// Copyright (C) 2013  Jens Rantil
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

// Contains the ZeroMQ server loop. Deals with incoming requests and
// delegates them to the event store. Also publishes newly stored events
// using a PUB socket.
//
// See README file for an up-to-date documentation of the ZeroMQ wire
// format.
package server

import (
	"bytes"
	"errors"
	"log"
	"container/list"
	"time"
	"sync"
	zmq "github.com/alecthomas/gozmq"
	"github.com/JensRantil/gorewind/eventstore"
)

// StartParams are parameters required for starting the server. 
type InitParams struct {
	// The event store to use as backend.
	Store *eventstore.EventStore
	// The ZeroMQ path that the command receiving socket will bind
	// to.
	CommandSocketZPath *string
	// The ZeroMQ path that the event publishing socket will bind
	// to.
	EvPubSocketZPath *string
	// ZeroMQ context to use. While the context potentially could be
	// instantiated by Server, it is not. Otherwise, it wuold be
	// impossible to use inproc:// endpoints.
	ZMQContext *zmq.Context
}

// Check all required initialization parameters are set.
func checkAllInitParamsSet(p *InitParams) error {
	if p.Store == nil {
		return errors.New("Missing param: Store")
	}
	if p.ZMQContext == nil {
		return errors.New("Missing ZeroMQ context.")
	}
	if p.CommandSocketZPath == nil {
		return errors.New("Missing param: CommandSocketZPath")
	}
	if p.EvPubSocketZPath == nil {
		return errors.New("Missing param: EvPubSocketZPath")
	}
	return nil
}

// A server instance. Can be run.
type Server struct {
	params InitParams

	evpubsock *zmq.Socket
	commandsock *zmq.Socket
	context *zmq.Context

	runningMutex sync.Mutex
	running bool
	stopChan chan bool
	waiter sync.WaitGroup
}

// IsRunning returns true if the server is running, false otherwise.
func (v *Server) IsRunning() bool {
	v.runningMutex.Lock()
	defer v.runningMutex.Unlock()
	return v.running
}

func (v* Server) Wait() {
	v.waiter.Wait()
}

// Stop stops a running server. Blocks until the server is stopped. If
// the server is not running, an error is returned.
//
// Call Server.Close() if you are done with the server.
func (v* Server) Stop() error {
	if !v.IsRunning() {
		return errors.New("Server not running.")
	}

	select {
	case v.stopChan <- true:
	default:
		return errors.New("Stop already signalled.")
	}
	v.Wait()
	// v.running is modified by Server.Run(...)

	if v.IsRunning() {
		return errors.New("Signalled stopped, but never stopped.")
	}

	return nil
}

// Initialize a new event store server and return a handle to it. The
// event store is not started. It's up to the caller to execute Run()
// on the server handle.
func New(params *InitParams) (*Server, error) {
	if params == nil {
		return nil, errors.New("Missing init params")
	}
	if err := checkAllInitParamsSet(params); err != nil {
		return nil, err
	}

	server := Server{
		params: *params,
		running: false,

		// Using buffered channel of one (1) to properly check
		// whether something has previously been buffered to
		// this channel using select/default. See
		// `Server.Stop()` for an example explanation.
		stopChan: make(chan bool, 1),
	}

	var allOkay *bool = new(bool)
	*allOkay = false
	defer func() {
		if (!*allOkay) {
			server.Close()
		}
	}()

	server.context = params.ZMQContext

	commandsock, err := server.context.NewSocket(zmq.ROUTER)
	if err != nil {
		return nil, err
	}
	server.commandsock = commandsock
	err = commandsock.Bind(*params.CommandSocketZPath)
	if err != nil {
		return nil, err
	}

	evpubsock, err := server.context.NewSocket(zmq.PUB)
	if err != nil {
		return nil, err
	}
	server.evpubsock = evpubsock
	if binderr := evpubsock.Bind(*params.EvPubSocketZPath); binderr != nil {
		return nil, binderr
	}

	*allOkay = true

	return &server, nil
}

// Clean up and server and deallocate resources.
func (v *Server) Close() error {
	if v.evpubsock != nil {
		if err := (*v.evpubsock).Close(); err != nil {
			return err
		}
		v.evpubsock = nil
	}
	if v.commandsock != nil {
		if err := (*v.commandsock).Close(); err != nil {
			return err
		}
		v.commandsock = nil
	}
	if v.context != nil {
		v.context.Close()
		v.context = nil
	}
	return nil
}

func (v *Server) setRunningState(newState bool) error {
	v.runningMutex.Lock()
	defer v.runningMutex.Unlock()
	if v.running == newState {
		return errors.New("Already is in running state.")
	}
	v.running = newState
	return nil
}

// Runs the server that distributes requests to workers.
// Panics on error since it is an essential piece of code required to
// run the application correctly.
func (v *Server) Start() error {
	v.waiter.Add(1)
	if err := v.setRunningState(true); err != nil {
		v.waiter.Done()
		return err
	}
	go func() {
		defer v.waiter.Done()
		defer v.setRunningState(false)
		loopServer((*v).params.Store, *(*v).evpubsock, *(*v).commandsock, v.stopChan)
	}()
	return nil
}

// The result of an asynchronous zmq.Poll call.
type zmqPollResult struct {
	err error
}

// Polls a bunch of ZeroMQ sockets and notifies the result through a
// channel. This makes it possible to combine ZeroMQ polling with Go's
// own built-in channels.
func asyncPoll(notifier chan zmqPollResult, items zmq.PollItems, stop chan bool) {
	for {
		timeout := time.Duration(1)*time.Second
		count, err := zmq.Poll(items, timeout)
		if count > 0 || err != nil {
			notifier <- zmqPollResult{err}
		}

		select {
		case <-stop:
			stop <- true
			return
		default:
		}
	}
}

func stopPoller(cancelChan chan bool) {
	cancelChan <- true
	<-cancelChan
}

// The core ZeroMQ messaging loop. Handles requests and responses
// asynchronously using the router socket. Every request is delegated to
// a goroutine for maximum concurrency.
//
// `gozmq` does currently not support copy-free messages/frames. This
// means that every message passing through this function needs to be
// copied in-memory. If this becomes a bottleneck in the future,
// multiple router sockets can be hooked to this final router to scale
// message copying.
//
// TODO: Make this a type function of `Server` to remove a lot of
// parameters.
func loopServer(estore *eventstore.EventStore, evpubsock, frontend zmq.Socket,
stop chan bool) {
	toPoll := zmq.PollItems{
		zmq.PollItem{Socket: &frontend, zmq.Events: zmq.POLLIN},
	}

	pubchan := make(chan eventstore.StoredEvent)
	estore.RegisterPublishedEventsChannel(pubchan)
	go publishAllSavedEvents(pubchan, evpubsock)
	defer close(pubchan)

	pollchan := make(chan zmqPollResult)
	respchan := make(chan zMsg)

	pollCancel := make(chan bool)
	defer stopPoller(pollCancel)

	go asyncPoll(pollchan, toPoll, pollCancel)
	for {
		select {
		case res := <-pollchan:
			if res.err != nil {
				log.Println("Could not poll:", res.err)
			}
			if res.err == nil && toPoll[0].REvents&zmq.POLLIN != 0 {
				msg, _ := toPoll[0].Socket.RecvMultipart(0)
				zmsg := zMsg(msg)
				go handleRequest(respchan, estore, zmsg)
			}
			go asyncPoll(pollchan, toPoll, pollCancel)
		case frames := <-respchan:
			if err := frontend.SendMultipart(frames, 0); err != nil {
				log.Println(err)
			}
		case <- stop:
			log.Println("Server asked to stop. Stopping...")
			return
		}
	}
}

// Publishes stored events to event listeners.
//
// Pops previously stored messages off a channel and published them to a
// ZeroMQ socket.
func publishAllSavedEvents(toPublish chan eventstore.StoredEvent, evpub zmq.Socket) {
	msg := make(zMsg, 3)
	for stored := range(toPublish) {
		msg[0] = stored.Event.Stream
		msg[1] = stored.Id
		msg[2] = stored.Event.Data

		if err := evpub.SendMultipart(msg, 0); err != nil {
			log.Println(err)
		}
	}
}

// A single frame in a ZeroMQ message.
type zFrame []byte

// A ZeroMQ message.
//
// I wish it could have been `[]zFrame`, but that would make conversion
// from `[][]byte` pretty messy[1].
//
// [1] http://stackoverflow.com/a/15650327/260805
type zMsg [][]byte

// Handles a single ZeroMQ RES/REQ loop synchronously.
//
// The full request message stored in `msg` and the full ZeroMQ response
// is pushed to `respchan`. The function does not return any error
// because it is expected to be called asynchronously as a goroutine.
func handleRequest(respchan chan zMsg, estore *eventstore.EventStore, msg zMsg) {

	// TODO: Rename to 'framelist'
	parts := list.New()
	for _, msgpart := range msg {
		parts.PushBack(msgpart)
	}

	resptemplate := list.New()
	emptyFrame := zFrame("")
	for true {
		resptemplate.PushBack(parts.Remove(parts.Front()))

		if bytes.Equal(parts.Front().Value.(zFrame), emptyFrame) {
			break
		}
	}

	if parts.Len() == 0 {
		errstr := "Incoming command was empty. Ignoring it."
		log.Println(errstr)
		response := copyList(resptemplate)
		response.PushBack(zFrame("ERROR " + errstr))
		respchan <- listToFrames(response)
		return
	}

	command := string(parts.Front().Value.(zFrame))
	switch command {
	case "PUBLISH":
		parts.Remove(parts.Front())
		if parts.Len() != 2 {
			// TODO: Constantify this error message
			errstr := "Wrong number of frames for PUBLISH."
			log.Println(errstr)
			response := copyList(resptemplate)
			response.PushBack(zFrame("ERROR " + errstr))
			respchan <- listToFrames(response)
		} else {
			estream := parts.Remove(parts.Front())
			data := parts.Remove(parts.Front())
			newevent := eventstore.Event{
				estream.(eventstore.StreamName),
				data.(zFrame),
			}
			newId, err := estore.Add(newevent)
			if err != nil {
				sErr := err.Error()
				log.Println(sErr)

				response := copyList(resptemplate)
				response.PushBack(zFrame("ERROR " + sErr))
				respchan <- listToFrames(response)
			} else {
				// the event was added
				response := copyList(resptemplate)
				response.PushBack(zFrame("PUBLISHED"))
				response.PushBack(zFrame(newId))
				respchan <- listToFrames(response)
			}
		}
	case "QUERY":
		parts.Remove(parts.Front())
		if parts.Len() != 3 {
			// TODO: Constantify this error message
			errstr := "Wrong number of frames for QUERY."
			log.Println(errstr)
			response := copyList(resptemplate)
			response.PushBack(zFrame("ERROR " + errstr))
			respchan <- listToFrames(response)
		} else {
			estream := parts.Remove(parts.Front())
			fromid := parts.Remove(parts.Front())
			toid := parts.Remove(parts.Front())

			req := eventstore.QueryRequest{
				Stream: estream.(zFrame),
				FromId: fromid.(zFrame),
				ToId: toid.(zFrame),
			}
			events, err := estore.Query(req)

			if err != nil {
				sErr := err.Error()
				log.Println(sErr)

				response := copyList(resptemplate)
				response.PushBack(zFrame("ERROR " + sErr))
				respchan <- listToFrames(response)
			} else {
				for eventdata := range(events) {
					response := copyList(resptemplate)
					response.PushBack([]byte("EVENT"))
					response.PushBack(eventdata.Id)
					response.PushBack(eventdata.Data)

					respchan <- listToFrames(response)
				}
				response := copyList(resptemplate)
				response.PushBack(zFrame("END"))
				respchan <- listToFrames(response)
			}
		}
	default:
		// TODO: Move these error strings out as constants of
		//       this package.

		// TODO: Move the chunk of code below into a separate
		// function and reuse for similar piece of code above.
		// TODO: Constantify this error message
		errstr := "Unknown request type."
		log.Println(errstr)
		response := copyList(resptemplate)
		response.PushBack(zFrame("ERROR " + errstr))
		respchan <- listToFrames(response)
	}
}

// Convert a doubly linked list of message frames to a slice of message
// fram
func listToFrames(l *list.List) zMsg {
	frames := make(zMsg, l.Len())
	i := 0
	for e := l.Front(); e != nil; e = e.Next() {
		frames[i] = e.Value.(zFrame)
	}
	return frames
}

// Helper function for copying a doubly linked list.
func copyList(l *list.List) *list.List {
	replica := list.New()
	replica.PushBackList(l)
	return replica
}

