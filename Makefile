all: server

IMAGE=duglin/airport-controller

server: *.go
	go fmt
	go build -ldflags "-w -extldflags -static" -tags netgo \
		-installsuffix netgo -o server

push: .push
.push: server *html *js Dockerfile
	docker build -t $(IMAGE) .
	docker push $(IMAGE)
	touch .push

clean:
	rm -f server .push
