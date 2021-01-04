# moin


go get -d github.com/remmerw/moin

cd $GOPATH/src/github.com/remmerw/moin

go mod vendor

go mod tidy

cd $HOME

set GO111MODULE=off

gomobile bind -o moin-1.0.0.aar -v -androidapi=24 -target=android github.com/remmerw/moin
