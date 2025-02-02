// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package tool

import (
	"context"
	"flag"
	"io/ioutil"
)

func (c *Cmd) upgrade(ctx context.Context, args ...string) {
	flags := flag.NewFlagSet("upgrade", flag.ExitOnError)
	help := `Upgrade Reflow's configuration and underlying services.`
	c.Parse(flags, args, help, "upgrade")
	if flags.NArg() != 0 {
		flags.Usage()
	}
	err := c.Config.Setup()
	if err != nil {
		c.Fatal(err)
	}
	b, err := c.Config.Marshal(false)
	if err != nil {
		c.Fatal(err)
	}
	if err := ioutil.WriteFile(c.ConfigFile, b, 0666); err != nil {
		c.Fatal(err)
	}
}
