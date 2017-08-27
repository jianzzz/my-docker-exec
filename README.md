Build
```
go get github.com/jianzzz/my-docker-exec
cd $GOPATH/src/github.com/jianzzz/my-docker-exec
go build -o myexec .
```

Run
```
./myexec -remote=127.0.0.1:2375 -version=v1.24 -container=test -cmd=/bin/sh
```

External dependency
```
"golang.org/x/net/context"
"golang.org/x/sys/unix"
```

My exec implement refers to docker 1.12.x.   
You can see from here: https://github.com/moby/moby/blob/1.12.x/cmd/docker/docker.go.

And I add token in request header, so you can use github.com/jianzzz/casbin-authz-plugin to authz.


