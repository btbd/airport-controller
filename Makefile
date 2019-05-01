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

run: server
	./server -p 93 -u amqp://$(USER):$(PASSWORD)@srcdog.com:9999/
	# ./server -p 93 -u amqp://$(USER):$(PASSWORD)@localhost:5672/

clean:
	rm -f server .push
