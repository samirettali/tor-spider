## General
- [ ] Save all data on SIGINT
- [x] Change all `os.Getenv` to `os.LookupEnv`
- [ ] Implement graph
- [X] Use logger object

## Concurrency
- [X] Fix `msgChan` and `errChan` sizes in order to prevent deadlock

## Storage
- [x] Make `PageStorage` an interface
- [x] Refactor `ElasticPageStorage`
- [ ] Make `ElasticPageStorage` concurrent
- [ ] Make `MongoJobsStorage` concurrent
- [ ] Store responses headers
- [ ] Save pages in case of error

## Collectors
- [ ] Make another collector for URLs added from the webserver, in order to be
    able to crawl clearnet and subreddits
