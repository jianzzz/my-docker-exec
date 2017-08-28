Build
```
go get github.com/jianzzz/my-docker-exec
cd $GOPATH/src/github.com/jianzzz/my-docker-exec
go build -o myexec .
```

Run
```
./myexec -remote=127.0.0.1:2375 -version=v1.24 -container=test -cmd=/bin/sh
./myexec -remote=/var/run/docker.sock -protocal=unix -version=v1.24 -container=test -cmd=/bin/sh
```

Dial connects to the address on the named network.
Known networks are "tcp", "tcp4" (IPv4-only), "tcp6" (IPv6-only), "udp", "udp4" (IPv4-only), "udp6" (IPv6-only), "ip", "ip4" (IPv4-only), "ip6" (IPv6-only), "unix", "unixgram" and "unixpacket".

External dependency
```
"golang.org/x/net/context"
"golang.org/x/sys/unix"
```

My exec implement refers to docker 1.12.x.   
You can see from here: https://github.com/moby/moby/blob/1.12.x/cmd/docker/docker.go.

And I add token in request header, so you can use github.com/jianzzz/casbin-authz-plugin to authz.


