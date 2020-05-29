docker:
	docker build -t tor-spider .

docker-run:
	docker-compose up spider

build:
	go build

run:
	bash -c "source local-env && ./tor-spider -v -b blacklist.txt"
