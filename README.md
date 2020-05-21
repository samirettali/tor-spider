# tor-spider

A spider for Hidden Services that uses the excellent
[colly](https://github.com/gocolly/colly) library to collect web pages.

It uses Elastic Search to save the pages.

You can use Docker to run it, just build the image with `make docker` and run
the containers with `make run`.
After it's running, you can add an url to start from with `curl
http://localhost:8888?url=<URL>`
You will need a [multitor](https://github.com/evait-security/docker-multitor)
container to run it.

Everything is a WIP and there are a lot of things that needs to be fixed or
implemented.
