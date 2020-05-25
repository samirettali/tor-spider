## General
- [x] Change all `os.Getenv` to `os.LookupEnv`
- [ ] Implement graph
- [X] Use logger object

## Concurrency
- [X] Fix `msgChan` and `errChan` sizes in order to prevent deadlock

## Storage
- [x] Make `PageStorage` an interface
- [x] Refactor `ElasticPageStorage`
- [ ] Save all data on SIGINT
- [ ] Make `ElasticPageStorage` concurrent
- [ ] Make `MongoJobsStorage` concurrent
- [ ] Store responses headers
- [ ] Save pages in case of error
- [ ] Save timed out links and the number of times it timed out, use it to
    revisit pages
- [X] Open connections only when needed

## Collectors
- [x] Make another collector for URLs added from the webserver, in order to be
    able to crawl clearnet and subreddits
- [x] Merge getCollector function and use a flag to get an onion one or a normal
    one
