# tor-spider

A spider for tor that uses the excellent
[colly](https://github.com/gocolly/colly) library to collect web pages.

It uses Elastic Search to save the pages.

Run with `make docker` and `make run` and the add an url with `curl
http://localhost:8888?url=<URL>`

Everything is a WIP and there are a lot of things that needs to be fixed or
implemented.
