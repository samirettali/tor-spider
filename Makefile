docker:
	docker build -t tor-spider .

run:
	docker-compose up

build-local:
	go build .

run-local:
	./tor-spider -v -w 32 -p 4 -d 2 -b blacklist.txt
