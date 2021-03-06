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

package hugot_test

import (
	"flag"
	"os"

	"context"

	"github.com/golang/glog"

	"github.com/tcolgate/hugot"

	// Pick an adapter to talk to an outside network
	"github.com/tcolgate/hugot/adapters/slack"

	// Pick some handlers.
	"github.com/tcolgate/hugot/handlers/ping"
	"github.com/tcolgate/hugot/handlers/tableflip"
	"github.com/tcolgate/hugot/handlers/testcli"
	"github.com/tcolgate/hugot/handlers/testweb"
)

func Example() {
	slackToken := flag.String("token", os.Getenv("SLACK_TOKEN"), "Slack API Token")
	nick := flag.String("nick", "minion", "Bot nick")
	flag.Parse()

	// The context can be used to shutdown the bot and any
	// Background handlers gracefully.
	ctx := context.Background()
	a, err := slack.New(*slackToken, *nick)
	if err != nil {
		glog.Fatal(err)
	}

	hugot.Handle(ping.New())
	hugot.Handle(tableflip.New())
	hugot.Handle(testcli.New())

	hugot.HandleHTTP(testweb.New())

	// This will start read , process and forward
	// messages from the adapter into the default
	// handler mux./
	hugot.ListenAndServe(ctx, nil, a)
}
