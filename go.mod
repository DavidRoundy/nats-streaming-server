module github.com/nats-io/nats-streaming-server

replace github.com/nats-io/go-nats => github.com/nats-io/nats.go v1.8.1

require (
	github.com/go-sql-driver/mysql v1.4.1
	github.com/gogo/protobuf v1.2.1
	github.com/hashicorp/go-msgpack v0.5.5
	github.com/hashicorp/raft v1.1.0
	github.com/lib/pq v1.1.1
	github.com/nats-io/nats-server/v2 v2.0.2
	github.com/nats-io/nats.go v1.8.1
	github.com/nats-io/nuid v1.0.1
	github.com/nats-io/stan.go v0.4.5
	github.com/prometheus/procfs v0.0.2
	go.etcd.io/bbolt v1.3.2
	golang.org/x/crypto v0.0.0-20190701094942-4def268fd1a4
	golang.org/x/sys v0.0.0-20190710143415-6ec70d6a5542
	google.golang.org/appengine v1.6.0 // indirect
)
