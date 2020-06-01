package spider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TODO make it return two channels, one for input and one for output

// MongoJobsStorage is an implementation of the JobsStorage interface
type MongoJobsStorage struct {
	DatabaseName   string
	CollectionName string
	Logger         *log.Logger
	URI            string
	BufferSize     int

	jobs    chan Job
	done    chan struct{}
	filling chan struct{}
	wg      *sync.WaitGroup
	min     int
}

// NewMongoJobsStorage returns an instance
func NewMongoJobsStorage(URI string, dbName string, colName string, bufSize int, min int) *MongoJobsStorage {
	s := &MongoJobsStorage{
		URI:            URI,
		DatabaseName:   dbName,
		CollectionName: colName,
		jobs:           make(chan Job, bufSize),
	}
	s.done = make(chan struct{})
	s.filling = make(chan struct{}, 1)
	s.min = min
	s.wg = &sync.WaitGroup{}
	return s
}

// Start does nothing in this case
func (s *MongoJobsStorage) Start() {
}

// Stop signals to the getter and the saver to stop
func (s *MongoJobsStorage) Stop() error {
	// TODO close jobs channel?
	close(s.done)
	s.wg.Wait()
	_, err := s.flush(cap(s.jobs))
	return err
}

// Status returns the status
func (s *MongoJobsStorage) Status() string {
	var b strings.Builder
	b.Grow(40)

	select {
	case <-s.done:
		fmt.Fprint(&b, "Stopped. ")
	default:
		fmt.Fprint(&b, "Running. ")
	}
	fmt.Fprintf(&b, "There are %d cached jobs ", len(s.jobs))
	count, err := s.countJobsInDb()

	if err != nil {
		fmt.Fprintf(&b, "and db is unreachable %v", err)
	} else {
		fmt.Fprintf(&b, "and %d jobs in the db", count)
	}

	return b.String()
}

// GetJob returns a job if it's cached in the jobs channel, otherwise, it caches
// jobs from the database if there is no other goroutine doing it. It timeouts
// after 3 seconds.
func (s *MongoJobsStorage) GetJob() (Job, error) {
	select {
	case job := <-s.jobs:
		return job, nil
	default:
		select {
		case s.filling <- struct{}{}:
			s.Logger.Debug("Started filler")
			s.fillCache()
			s.Logger.Debug("Ended filler")
			<-s.filling
		default:
			s.Logger.Debug("Filler already running")
		}
		select {
		case job := <-s.jobs:
			return job, nil
		case <-time.After(3 * time.Second):
			return Job{}, &NoJobsError{"No jobs"}
		}
	}
}

// SaveJob adds a job to the channel if it's not full, otherwise it flushes the
// channel to the db and then adds the job to the channel.
func (s *MongoJobsStorage) SaveJob(job Job) {
	select {
	case s.jobs <- job:
		return
	default:
		n := len(s.jobs) - s.min
		s.flush(n)
		s.jobs <- job
	}
}

func (s *MongoJobsStorage) getCollectionClient() (*mongo.Collection, error) {
	var client *mongo.Client
	var err error

	if client, err = mongo.NewClient(options.Client().ApplyURI(s.URI)); err != nil {
		return nil, err
	}

	if err = client.Connect(context.Background()); err != nil {
		return nil, err
	}

	db := client.Database(s.DatabaseName)
	return db.Collection(s.CollectionName), nil
}

func (s *MongoJobsStorage) fillCache() int {
	var col *mongo.Collection
	var err error
	if col, err = s.getCollectionClient(); err != nil {
		s.Logger.Error(err)
		return 0
	}

	defer func() {
		ctx := context.Background()
		if err := col.Database().Client().Disconnect(ctx); err != nil {
			s.Logger.Error(err)
		}
	}()

	findOptions := options.Find()
	findOptions.SetLimit(int64(s.min))
	cur, err := col.Find(context.Background(), bson.D{}, findOptions)

	if err != nil {
		if !errors.Is(err, mongo.ErrNoDocuments) {
			s.Logger.Error(err)
		}
		return 0
	}

	count := 0
	// TODO fixed size array?
	toDelete := make([]string, 0)
	for cur.Next(context.Background()) {
		var job Job
		err := cur.Decode(&job)
		if err != nil {
			s.Logger.Error(err)
		} else {
			count++
			s.jobs <- job
			toDelete = append(toDelete, job.URL)
		}
	}

	if err := cur.Err(); err != nil {
		log.Fatal(err)
	}

	cur.Close(context.Background())

	filter := bson.M{"url": bson.M{"$in": toDelete}}
	res, err := col.DeleteMany(context.Background(), filter)
	if err != nil {
		s.Logger.Error(err)
	}
	s.Logger.Debugf("Got %d jobs, deleted %d jobs", count, res.DeletedCount)

	return count

}

func (s *MongoJobsStorage) flush(max int) (int, error) {
	jobs := make([]interface{}, 0)
loop:
	for i := 0; i < max; i++ {
		select {
		case job := <-s.jobs:
			jobs = append(jobs, job)
		default:
			break loop
		}
	}

	if len(jobs) == 0 {
		return 0, nil
	}

	ctx := context.Background()
	col, err := s.getCollectionClient()
	if err != nil {
		// TODO save jobs
		return 0, err
	}
	result, err := col.InsertMany(ctx, jobs)
	if err != nil {
		return 0, err

	}
	inserted := len(result.InsertedIDs)
	if inserted != len(jobs) {
		s.Logger.Errorf("%d jobs were not saved", len(jobs)-inserted)
	}
	return inserted, nil
}

func (s *MongoJobsStorage) countJobsInDb() (int64, error) {
	col, err := s.getCollectionClient()
	if err != nil {
		return 0, err
	}
	defer col.Database().Client().Disconnect(context.Background())
	count, err := col.CountDocuments(context.Background(), bson.D{})
	if err != nil {
		return 0, err
	}
	return count, nil
}
