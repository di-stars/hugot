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
	"net/url"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"

	"context"

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

type errNextCommand struct {
	ctx context.Context
}

func (e errNextCommand) Error() string {
	return "use next command"
}

// ErrNextCommand is returned if the command wishes the message
// to be passed to one of the sub-ommands of a CommandSet. The
// ctx will be passed through to the next CommandHandler
func ErrNextCommand(ctx context.Context) error {
	return errNextCommand{ctx}
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

	Copy() ResponseWriter // Returns a copy of this response writer
}

type responseWriter struct {
	snd Sender
	msg Message
	an  string
}

func newResponseWriter(s Sender, m Message, an string) ResponseWriter {
	return &responseWriter{s, m, an}
}

// ResponseWriterFromContext constructs a ResponseWriter from the adapter
// stored in the context. A destination Channel/User must be set to send
// messages..
func ResponseWriterFromContext(ctx context.Context) (ResponseWriter, bool) {
	s, ok := SenderFromContext(ctx)
	if !ok {
		return nil, false
	}
	an := fmt.Sprintf("%T", s)
	return newResponseWriter(s, Message{}, an), true
}

// Write implements the io.Writer interface. All writes create a single
// new message that is then sent to the ResoneWriter's current adapter
func (w *responseWriter) Write(bs []byte) (int, error) {
	nmsg := w.msg
	nmsg.Text = string(bs)
	w.Send(context.TODO(), &nmsg)
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
	messagesTx.WithLabelValues(w.an, m.Channel, m.From).Inc()
	w.snd.Send(ctx, m)
}

// Copy returns a copy of this response writer
func (w *responseWriter) Copy() ResponseWriter {
	return &responseWriter{w.snd, Message{}, w.an}
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
	return newResponseWriter(nullSender{}, m, "null")
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
	ProcessMessage(ctx context.Context, w ResponseWriter, m *Message) error
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

func (brh *baseRawHandler) ProcessMessage(ctx context.Context, w ResponseWriter, m *Message) error {
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
// ErrNextCommand(ctx) can be returned to inform the command mux to hand the resulting
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

type byAlpha struct {
	ns  []string
	ds  []string
	chs []CommandHandler
}

func (b *byAlpha) Len() int           { return len(b.ns) }
func (b *byAlpha) Less(i, j int) bool { return b.ns[i] < b.ns[j] }
func (b *byAlpha) Swap(i, j int) {
	b.ns[i], b.ns[j] = b.ns[j], b.ns[i]
	b.ds[i], b.ds[j] = b.ns[j], b.ds[i]
	b.chs[i], b.chs[j] = b.chs[j], b.chs[i]
}

// List returns the names and usage of the subcommands of
// a CommandSet.
func (cs *CommandSet) List() ([]string, []string, []CommandHandler) {
	cmds := []string{}
	descs := []string{}
	chs := []CommandHandler{}
	hasHelp := false

	for _, ch := range *cs {
		n, d := ch.Describe()
		if n == "help" {
			hasHelp = true
			continue
		}
		cmds = append(cmds, n)
		descs = append(descs, d)
		chs = append(chs, ch)
	}

	sorted := &byAlpha{cmds, descs, chs}
	sort.Sort(sorted)
	if hasHelp {
		hh := (*cs)["help"]
		_, hd := hh.Describe()
		sorted.ns = append([]string{"help"}, sorted.ns...)
		sorted.ds = append([]string{hd}, sorted.ds...)
		sorted.chs = append([]CommandHandler{hh}, sorted.chs...)
	}

	return sorted.ns, sorted.ds, sorted.chs
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
		cmds, _, _ := cs.List()
		return fmt.Errorf("required sub-command missing: %s", strings.Join(cmds, ", "))
	}

	matches := []CommandHandler{}
	matchesns := []string{}
	ematches := []CommandHandler{}
	for name, cmd := range *cs {
		if strings.HasPrefix(name, m.args[0]) {
			matches = append(matches, cmd)
			matchesns = append(matchesns, name)
		}
		if name == m.args[0] {
			ematches = append(ematches, cmd)
		}
	}
	if len(matches) == 0 && len(ematches) == 0 {
		return ErrUnknownCommand
	}
	if len(ematches) > 1 {
		return fmt.Errorf("multiple exact matches for %s", m.args[0])
	}
	if len(ematches) == 1 {
		return runCommandHandler(ctx, ematches[0], w, m)
	}
	if len(matches) == 1 {
		return runCommandHandler(ctx, matches[0], w, m)
	}
	return fmt.Errorf("ambigious command, %s: %s", m.args[0], strings.Join(matchesns, ", "))
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
	return ErrNextCommand(ctx)
}

// NewCommandHandler wraps the given function f as a CommandHandler with the
// provided name and description.
func NewCommandHandler(name, desc string, f CommandFunc, cs *CommandSet) CommandWithSubsHandler {
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
	var errnc errNextCommand
	var ok bool
	err := bch.bcf(ctx, w, m)
	if errnc, ok = err.(errNextCommand); !ok {
		return err
	}
	return bch.subs.NextCommand(errnc.ctx, w, m)
}

func (bch *baseCommandHandler) SubCommands() *CommandSet {
	return bch.subs
}

// WebHookHandler handlers are used to expose a registered handler via a web server.
// The SetURL method is called to inform the handler what it's external URL will be.
// This will normally be done by the Mux. Other handlers can use URL to generate
// links suitable for external use.
// You can use the http.Handler Request.Context() to get a ResponseWriter to write
// into the bots adapters. You need to SetChannel the resulting ResponseWriter to
// send messages.
type WebHookHandler interface {
	Handler
	URL() *url.URL      // Is called to retrieve the location of the Handler
	SetURL(*url.URL)    // Is called after the WebHook is added, to inform it where it lives
	SetAdapter(Adapter) // Is called to set the default adapter for this handler to use
	http.Handler
}

// WebHookHandlerFunc describes the called convention for a WebHook.
type WebHookHandlerFunc func(ctx context.Context, hw ResponseWriter, w http.ResponseWriter, r *http.Request)

type baseWebHookHandler struct {
	ctx context.Context
	a   Adapter
	Handler
	hf  http.HandlerFunc
	url *url.URL
}

// ServeHTTP  implement the http.Handler interface for a baseWebHandler
func (bwhh *baseWebHookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ctx = NewAdapterContext(ctx, bwhh.a)
	r = r.WithContext(ctx)

	bwhh.hf(w, r)
}

// NewWebHookHandler creates a new WebHookHandler provided name and description.
func NewWebHookHandler(name, desc string, hf http.HandlerFunc) WebHookHandler {
	return &baseWebHookHandler{
		Handler: newBaseHandler(name, desc),
		url:     &url.URL{},
		hf:      hf,
	}
}

func (bwhh *baseWebHookHandler) SetURL(u *url.URL) {
	if glog.V(2) {
		glog.Infof("SetURL to %s", *u)
	}
	bwhh.url = u
}

func (bwhh *baseWebHookHandler) URL() *url.URL {
	return bwhh.url
}

func (bwhh *baseWebHookHandler) SetAdapter(a Adapter) {
	if glog.V(3) {
		glog.Infof("WebHander adapter set to %#v", a)
	}
	bwhh.a = a
}

func glogPanic() {
	err := recover()
	if err != nil && err != flag.ErrHelp {
		glog.Error(err)
		glog.Error(string(debug.Stack()))
	}
}

// Loop processes messages from adapters a and as, and passes them
// to the provided handler h. ctx can be used to stop the processesing
// and inform any running handlers. WebHookHandlers and BackgroundHandlers
// will be configured to use a as the default handler
func Loop(ctx context.Context, h Handler, a Adapter, as ...Adapter) {
	an := fmt.Sprintf("%T", a)
	if bh, ok := h.(BackgroundHandler); ok {
		runBackgroundHandler(ctx, bh, newResponseWriter(a, Message{}, an))
	}

	if wh, ok := h.(WebHookHandler); ok {
		wh.SetAdapter(a)
	}

	type smrw struct {
		w ResponseWriter
		m *Message
	}
	mrws := make(chan smrw)

	for _, a := range append(as, a) {
		go func(a Adapter) {
			an := fmt.Sprintf("%T", a)
			for {
				select {
				case m := <-a.Receive():
					rw := newResponseWriter(a, *m, an)
					mrws <- smrw{rw, m}
				case <-ctx.Done():
					return
				}
			}
		}(a)
	}

	for {
		select {
		case mrw := <-mrws:
			if glog.V(3) {
				glog.Infof("Message: %#v", *mrw.m)
			}
			messagesRx.WithLabelValues(an, mrw.m.Channel, mrw.m.From).Inc()

			if rh, ok := h.(RawHandler); ok {
				go runRawHandler(ctx, rh, mrw.w, mrw.m)
			}

			if hh, ok := h.(HearsHandler); ok {
				go runHearsHandler(ctx, hh, mrw.w, mrw.m)
			}

			if ch, ok := h.(CommandHandler); ok {
				go runCommandHandler(ctx, ch, mrw.w, mrw.m)
			}
		case <-ctx.Done():
			return
		}
	}
}

// runBackgroundHandler starts the provided BackgroundHandler in a new
// go routine.
func runBackgroundHandler(ctx context.Context, h BackgroundHandler, w ResponseWriter) {
	glog.Infof("Starting background %v\n", h)
	go func(ctx context.Context, bh BackgroundHandler) {
		defer glogPanic()
		h.StartBackground(ctx, w)
	}(ctx, h)
}

// runRawHandler passing message m to the provided handler.  go routine.
func runRawHandler(ctx context.Context, h RawHandler, w ResponseWriter, m *Message) bool {
	defer glogPanic()
	h.ProcessMessage(ctx, w, m)

	return false
}

// runHearsHandler will match the go routine.
func runHearsHandler(ctx context.Context, h HearsHandler, w ResponseWriter, m *Message) bool {
	defer glogPanic()

	if mtchs := h.Hears().FindAllStringSubmatch(m.Text, -1); mtchs != nil {
		go h.Heard(ctx, w, m, mtchs)
		return true
	}
	return false
}

// runCommandHandler initializes the message m as a command message and passed
// it to the given handler.
func runCommandHandler(ctx context.Context, h CommandHandler, w ResponseWriter, m *Message) error {
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
