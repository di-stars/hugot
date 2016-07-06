// Copyright (c) 2016 Tristan Colgate-McFarlane
//
// This file is part of hugot.
//
// hugot is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// hugot is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with hugot.  If not, see <http://www.gnu.org/licenses/>.

package hugot

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"

	"golang.org/x/net/context"

	"github.com/golang/glog"
	"github.com/mattn/go-shellwords"
)

var (
	// ErrSkipHears is used by Command handlers to indicate they have
	// handled a mesage and that any following Hear handlers should be
	// skipped.
	ErrSkipHears = errors.New("skip hear messages")

	// ErrUnknownCommand is returned by a command mux if the command did
	// not match any of it's registered handlers.
	ErrUnknownCommand = errors.New("unknown command")

	// ErrBadCLI implies that we could not process this message as a
	// command line. E.g. due to potentially mismatched quoting or bad
	// escaping.
	ErrBadCLI = errors.New("could not process as command line")

	// ErrNextCommand is returned if the command wishes the message
	// to be passed to one of the sub-ommands of a CommandMux.
	ErrNextCommand = errors.New("call the next command")
)

// ErrUsage indicates that Command handler was used incorrectly. The
// string returned is a usage message generated by a call to -help
// for this command
type ErrUsage struct {
	string
}

// Error implements the Error interface for an ErrUsage.
func (e ErrUsage) Error() string {
	return e.string
}

// Describer returns the name and description of a handler. This
// is used to identify the handler within Command and HTTP Muxs,
// and to provide a descriptive name for the handler in help text.
type Describer interface {
	Describe() (string, string)
}

// Handler is a handler with no actual functionality
type Handler interface {
	Describer
}

// ResponseWriter is used to Send messages back to a user.
type ResponseWriter interface {
	Sender
	io.Writer

	SetChannel(c string) // Forces messages to a certain channel
	SetTo(to string)     // Forces messages to a certain user
	SetSender(a Sender)  // Forces messages to a different sender or adapter
}

type responseWriter struct {
	snd Sender
	msg Message
}

func newResponseWriter(s Sender, m Message) ResponseWriter {
	return &responseWriter{s, m}
}

// Write implements the io.Writer interface. All writes create a single
// new message that is then sent to the ResoneWriter's current adapter
func (w *responseWriter) Write(bs []byte) (int, error) {
	nmsg := w.msg
	nmsg.Text = string(bs)
	w.snd.Send(context.TODO(), &nmsg)
	return len(bs), nil
}

// SetChannel sets the outbound channel for message sent via this writer
func (w *responseWriter) SetChannel(s string) {
	w.msg.Channel = s
}

// SetChannel sets the target user for message sent via this writer
func (w *responseWriter) SetTo(s string) {
	w.msg.To = s
}

// SetSender sets the target adapter for sender sent via this writer
func (w *responseWriter) SetSender(s Sender) {
	w.snd = s
}

// Send implements the Sender interface
func (w *responseWriter) Send(ctx context.Context, m *Message) {
	w.snd.Send(ctx, m)
}

// nullSender is a sender which discards anything sent to it, this is
// useful for the help handler.
type nullSender struct {
}

// Send implements Send, and discards the message
func (nullSender) Send(ctx context.Context, m *Message) {
}

// NewNullResponseWriter creates a ResponseWriter that discards all
// message sent to it.
func NewNullResponseWriter(m Message) ResponseWriter {
	return newResponseWriter(nullSender{}, m)
}

type baseHandler struct {
	name string
	desc string
}

func (bh *baseHandler) Describe() (string, string) {
	return bh.name, bh.desc
}

func newBaseHandler(name, desc string) Handler {
	return &baseHandler{name, desc}
}

// RawHandler will recieve every message sent to the handler, without
// any filtering.
type RawHandler interface {
	Handler
	Handle(ctx context.Context, w ResponseWriter, m *Message) error
}

type baseRawHandler struct {
	Handler
	rhf RawFunc
}

// RawFunc describes the calling convention for RawHandler. m is the
// incoming message. Responses can be written to w.
type RawFunc func(ctx context.Context, w ResponseWriter, m *Message) error

// NewRawHandler will wrap the function f as a RawHandler with the name
// and description provided
func NewRawHandler(name, desc string, f RawFunc) RawHandler {
	return &baseRawHandler{
		Handler: newBaseHandler(name, desc),
		rhf:     f,
	}
}

func (brh *baseRawHandler) Handle(ctx context.Context, w ResponseWriter, m *Message) error {
	return brh.rhf(ctx, w, m)
}

// BackgroundHandler gets run when the bot starts listening. They are
// intended for publishing messages that are not in response to any
// specific incoming message.
type BackgroundHandler interface {
	Handler
	StartBackground(ctx context.Context, w ResponseWriter)
}

type baseBackgroundHandler struct {
	Handler
	bhf BackgroundFunc
}

// BackgroundFunc describes the calling convention for Background handlers
type BackgroundFunc func(ctx context.Context, w ResponseWriter)

// NewBackgroundHandler wraps f up as a BackgroundHandler with the name and
// description provided.
func NewBackgroundHandler(name, desc string, f BackgroundFunc) BackgroundHandler {
	return &baseBackgroundHandler{
		Handler: newBaseHandler(name, desc),
		bhf:     f,
	}
}

func (bbh *baseBackgroundHandler) StartBackground(ctx context.Context, w ResponseWriter) {
	bbh.bhf(ctx, w)
}

// HeardFunc describes the calling convention for a Hears handler.
type HeardFunc func(ctx context.Context, w ResponseWriter, m *Message, submatches [][]string) // Called once a message matches, and is passed any submatches from the regexp capture groups

// HearsHandler is a handler which responds to messages matching a specific
// pattern
type HearsHandler interface {
	Handler
	Hears() *regexp.Regexp                                                          // Returns the regexp we want to hear
	Heard(ctx context.Context, w ResponseWriter, m *Message, submatches [][]string) // Called once a message matches, and is passed any submatches from the regexp capture groups
}

type baseHearsHandler struct {
	Handler
	rgxp *regexp.Regexp
	hhf  HeardFunc
}

// NewHearsHandler wraps f as a Hears handler that reponnds to the regexp provided, with the given name a description
func NewHearsHandler(name, desc string, rgxp *regexp.Regexp, f HeardFunc) HearsHandler {
	return &baseHearsHandler{
		Handler: newBaseHandler(name, desc),
		rgxp:    rgxp,
		hhf:     f,
	}
}

func (bhh *baseHearsHandler) Hears() *regexp.Regexp {
	return bhh.rgxp
}

func (bhh *baseHearsHandler) Heard(ctx context.Context, w ResponseWriter, m *Message, submatches [][]string) {
	bhh.hhf(ctx, w, m, submatches)
}

// CommandFunc describes the calling convention for CommandHandler
type CommandFunc func(ctx context.Context, w ResponseWriter, m *Message) error

// CommandHandler handlers are used to implement CLI style commands. Before the
// Command method is called, the in the incoming message m will have the Text
// of the message parsed into invidual strings, accouting for quoting.
// m.Args(0) will be the name of the command as the handler was called, as per
// os.Args(). Command should add any requires falgs to m and then call m.Parse()
// ErrNextCommand can be returned to inform the command mux to hand the resulting
// Args to any known sub CommandHandler.
type CommandHandler interface {
	Handler
	Command(ctx context.Context, w ResponseWriter, m *Message) error
}

// CommandWithSubsHandler should be implemented by any command that includes
// sub commands.
type CommandWithSubsHandler interface {
	CommandHandler
	SubCommands() *CommandSet // List the supported sub-commands
}

// CommandSet assists with supporting command handlers with sub-commands.
type CommandSet map[string]CommandHandler

// NewCommandSet creates an empty commands set.
func NewCommandSet() *CommandSet {
	cs := make(CommandSet)
	return &cs
}

// AddCommandHandler adds a CommandHandler to a CommandSet
func (cs *CommandSet) AddCommandHandler(c CommandHandler) {
	n, _ := c.Describe()

	(*cs)[n] = c
}

// List returns the names and usage of the subcommands of
// a CommandSet.
func (cs *CommandSet) List() ([]string, []string) {
	cmds := []string{}
	descs := []string{}

	for _, ch := range *cs {
		n, d := ch.Describe()
		cmds = append(cmds, n)
		descs = append(descs, d)
	}

	return cmds, descs
}

// NextCommand picks the next commands to run from this command set based on the content
// of the message
func (cs *CommandSet) NextCommand(ctx context.Context, w ResponseWriter, m *Message) error {
	var err error

	// This is repeated from RunCommandHandler, probably something wrong there
	if m.args == nil {
		m.args, err = shellwords.Parse(m.Text)
		if err != nil {
			return ErrBadCLI
		}
	}
	if len(m.args) == 0 {
		cmds, _ := cs.List()
		return fmt.Errorf("required sub-command missing: %s", strings.Join(cmds, ", "))
	}

	if cmd, ok := (*cs)[m.args[0]]; ok {
		err = RunCommandHandler(ctx, cmd, w, m)
	} else {
		return ErrUnknownCommand
	}

	return err
}

type baseCommandHandler struct {
	Handler
	bcf  CommandFunc
	subs *CommandSet
}

func defaultCommandHandler(ctx context.Context, w ResponseWriter, m *Message) error {
	if err := m.Parse(); err != nil {
		return err
	}
	return ErrNextCommand
}

// NewCommandHandler wraps the given function f as a CommandHandler with the
// provided name and description.
func NewCommandHandler(name, desc string, f CommandFunc, cs *CommandSet) CommandHandler {
	if f == nil {
		f = defaultCommandHandler
	}
	return &baseCommandHandler{
		Handler: newBaseHandler(name, desc),
		bcf:     f,
		subs:    cs,
	}
}

func (bch *baseCommandHandler) Command(ctx context.Context, w ResponseWriter, m *Message) error {
	err := bch.bcf(ctx, w, m)
	if err != ErrNextCommand {
		return err
	}

	return bch.subs.NextCommand(ctx, w, m)
}

func (bch *baseCommandHandler) SubCommands() *CommandSet {
	return bch.subs
}

// WebHookHandler handlers are used to expose a registered handler via a web server.
type WebHookHandler interface {
	Handler
	ReceiveHTTP(ctx context.Context, hw ResponseWriter, w http.ResponseWriter, r *http.Request)
}

// WebHookHandlerFunc describes the called convention for a WebHook.
type WebHookHandlerFunc func(ctx context.Context, hw ResponseWriter, w http.ResponseWriter, r *http.Request)

type baseWebHookHandler struct {
	ctx context.Context
	a   Adapter
	Handler
	hf WebHookHandlerFunc
}

// NewWebHookPHandler creates a new WebHookHandler provided name and description.
func (bwhh *baseWebHookHandler) ReceiveHTTP(ctx context.Context, hw ResponseWriter, w http.ResponseWriter, r *http.Request) {
	bwhh.hf(ctx, hw, w, r)
}

// NewWebHookHandler creates a new WebHookHandler provided name and description.
func NewWebHookHandler(name, desc string, hf WebHookHandlerFunc) WebHookHandler {
	return &baseWebHookHandler{
		Handler: newBaseHandler(name, desc),
		hf:      hf,
	}
}

// NewNetHTTPHandlerFunc creates a new WebHookHandler with the http.HandlerFunc h, and the
// provided name and description. The response wrtier from the webhook handler is
// discarded
func NewNetHTTPHandlerFunc(name, desc string, h http.HandlerFunc) WebHookHandler {
	return &baseWebHookHandler{
		Handler: newBaseHandler(name, desc),
		hf: func(ctx context.Context, hw ResponseWriter, w http.ResponseWriter, r *http.Request) {
			h(w, r)
		},
	}
}

// NewNetHTTPHandler creates a new WebHookHandler with the http.Handler h, and the
// provided name and description. The response writer from the webhook handler is
// discarded
func NewNetHTTPHandler(name, desc string, h http.Handler) WebHookHandler {
	return &baseWebHookHandler{
		Handler: newBaseHandler(name, desc),
		hf: func(ctx context.Context, hw ResponseWriter, w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(w, r)
		},
	}
}

func glogPanic() {
	err := recover()
	if err != nil && err != flag.ErrHelp {
		glog.Error(err)
		glog.Error(string(debug.Stack()))
	}
}

// RunHandlers process messages from adapter a, and passes them
// to the provided handler h. ctx can be used to stop the processesing
// and  inform any running handlers. WARNING: probably not to be used
// directly and may be made private in the future.
func RunHandlers(ctx context.Context, h Handler, a Adapter) {
	if bh, ok := h.(BackgroundHandler); ok {
		RunBackgroundHandler(ctx, bh, newResponseWriter(a, Message{}))
	}

	for {
		select {
		case m := <-a.Receive():
			if rh, ok := h.(RawHandler); ok {
				go RunRawHandler(ctx, rh, newResponseWriter(a, *m), m)
			}

			if hh, ok := h.(HearsHandler); ok {
				go RunHearsHandler(ctx, hh, newResponseWriter(a, *m), m)
			}

			if ch, ok := h.(CommandHandler); ok {
				go RunCommandHandler(ctx, ch, newResponseWriter(a, *m), m)
			}
		case <-ctx.Done():
			return
		}
	}
}

// RunBackgroundHandler starts the provided BackgroundHandler in a new
// go routine. WARNING: probably not to be used
// directly and may be made private in the future.
func RunBackgroundHandler(ctx context.Context, h BackgroundHandler, w ResponseWriter) {
	glog.Infof("Starting background %v\n", h)
	go func(ctx context.Context, bh BackgroundHandler) {
		defer glogPanic()
		h.StartBackground(ctx, w)
	}(ctx, h)
}

// RunRawHandler passing message m to the provided handler.  go routine.
// WARNING: probably not to be used directly and may be made private in the
// future.
func RunRawHandler(ctx context.Context, h RawHandler, w ResponseWriter, m *Message) bool {
	defer glogPanic()
	h.Handle(ctx, w, m)

	return false
}

// RunHearsHandler will match the go routine. WARNING: probably not to be used
// directly and may be made private in the future.
func RunHearsHandler(ctx context.Context, h HearsHandler, w ResponseWriter, m *Message) bool {
	defer glogPanic()

	if mtchs := h.Hears().FindAllStringSubmatch(m.Text, -1); mtchs != nil {
		go h.Heard(ctx, w, m, mtchs)
		return true
	}
	return false
}

// RunCommandHandler initializes the message m as a command message and passed
// it to the given handler.WARNING: probably not to be used directly and may be
// made private in the future.
func RunCommandHandler(ctx context.Context, h CommandHandler, w ResponseWriter, m *Message) error {
	if h != nil && glog.V(2) {
		glog.Infof("RUNNING %v %v\n", h, m.args)
	}
	defer glogPanic()
	var err error

	if m.args == nil {
		m.args, err = shellwords.Parse(m.Text)
		if err != nil {
			return ErrBadCLI
		}
	}

	if len(m.args) == 0 {
		//nothing to do.
		return errors.New("command handler called with no possible arguments")
	}

	name := m.args[0]
	m.flagOut = &bytes.Buffer{}
	m.FlagSet = flag.NewFlagSet(name, flag.ContinueOnError)
	m.FlagSet.SetOutput(m.flagOut)

	err = h.Command(ctx, w, m)
	if err == flag.ErrHelp {
		fmt.Fprint(w, cmdUsage(h, name, nil).Error())
		return ErrSkipHears
	}

	return err
}
