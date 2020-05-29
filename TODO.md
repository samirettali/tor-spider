## General
- [x] Change all `os.Getenv` to `os.LookupEnv`
- [ ] Implement graph
- [ ] Improve env variables loading
- [X] Use logger object
- [x] Implement [service](https://rauljordan.com/2020/03/10/building-a-service-registry-in-go.html)
- [ ] Periodically check proxy health

## Concurrency
- [X] Fix `msgChan` and `errChan` sizes in order to prevent deadlock
- [ ] Improve goroutines tracking with waitgroups because right now it's a mess

## Storage
- [x] Make `PageStorage` an interface
- [x] Refactor `ElasticPageStorage`
- [x] Save all data on SIGINT
- [ ] Make `ElasticPageStorage` concurrent
- [ ] Make `MongoJobsStorage` concurrent
- [ ] Store responses headers
- [ ] Save pages in case of error
- [ ] Save timed out links and the number of times it timed out, use it to
    revisit pages
- [x] Open connections only when needed
- [ ] Organize data by domain

## Collectors
- [x] Make another collector for URLs added from the webserver, in order to be
    able to crawl clearnet and subreddits
- [x] Merge getCollector function and use a flag to get an onion one or a normal
    one
- [ ] Make some periodic collectors for places where links gets published
