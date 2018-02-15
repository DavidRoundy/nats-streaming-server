#!/bin/bash -e
# Run from directory above via ./scripts/cov.sh

rm -rf ./cov
mkdir cov
go test -v -covermode=count -coverprofile=./cov/server.out ./server
go test -v -covermode=count -coverprofile=./cov/stores.out ./stores
go test -v -covermode=count -coverprofile=./cov/stores_no_buffer.out -run=TestFS ./stores -no_buffer
go test -v -covermode=count -coverprofile=./cov/util.out ./util
gocovmerge ./cov/*.out > acc.out
rm -rf ./cov

# If we have an arg, assume travis run and push to coveralls. Otherwise launch browser results
if [[ -n $1 ]]; then
    $HOME/gopath/bin/goveralls -coverprofile=acc.out
    rm -rf ./acc.out
else
    go tool cover -html=acc.out
fi
