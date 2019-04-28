server: *.go
	go fmt & go build -ldflags "-w -extldflags -static" -tags netgo -installsuffix netgo -o server

push: .airport
.airport: server *html *js Dockerfile
	docker build -t duglin/airport .
	docker push duglin/airport
	touch .airport

clean:
	rm -f server