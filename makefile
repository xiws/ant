
.PHONY: build pub
build:
	go build ./cmd/ant
pub: build
	cp ./ant $(HOME)/go/bin/
