# tor-spider

A spider for Hidden Services that uses the excellent
[colly](https://github.com/gocolly/colly) library to collect web pages.

The default implementation uses Elastic Search to save the pages, mongoDB to store the URLs to be crawled and redis to check if a
URL has ben visited or not.

You can use Docker to run it, just build the image with `make docker` and run
the containers with `make run`.
After it's running, you can add a url to start from with `curl
http://localhost:8080?url=<URL>`

Everything is a WIP and there are a lot of things that needs to be fixed or
implemented.
