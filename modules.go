package main

import (
	_ "github.com/forestjohnsonpeoplenet/logspout/adapters/http"
  _ "github.com/forestjohnsonpeoplenet/logspout/adapters/syslogamqp"
	_ "github.com/gliderlabs/logspout/adapters/syslog"
	_ "github.com/gliderlabs/logspout/transports/tcp"
	_ "github.com/gliderlabs/logspout/transports/tls"
	_ "github.com/gliderlabs/logspout/transports/udp"
	_ "github.com/looplab/logspout-logstash"
)
