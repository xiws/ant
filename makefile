
.PHONY: build pub
build:
	go build ./cmd/ant
pub: build
	cp ./ant $(HOME)/go/bin/

.PHONY: clear
clear:
	rm -rf ./ant .ant/sessions .ant/logs
