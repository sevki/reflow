// Package dynamodbtask implements the taskdb.TaskDB interface for AWS dynamodb backend.
// Every run or task is stored in a row with their attributes which includes labels, user,
// keepalive and start times. Tasks have a runid column to identify which run it belongs to.
// Tasks also store the flowId of the reflow flow, resultId, exec uri and stdout, stderr
// and inspect log ids.
// To make common queries like recently run/tasks, runs/tasks have day time
// buckets stored. "Date-Keepalive-index" index allows querying runs/tasks based on time
// buckets. Dynamodbtask also uses a bunch of secondary indices to help with run/task querying.
// Schema:
// run:  {ID, ID4, Labels, Type="run",  StartTime, User, Keepalive}
// task: {ID, ID4, Labels, Type="task", StartTime, Keepalive, RunID, RunID4, FlowID, URI, ResultID}
// Indexes:
// 1. Date-Keepalive-index - for queries that are time based.
// 2. RunID-index - for find all tasks that belongs to a run.
// 3. ID-index and ID4-ID-index - for queries looking for specific runs or tasks.
package dynamodbtask

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbiface"
	"github.com/grailbio/base/digest"
	"github.com/grailbio/base/limiter"
	"github.com/grailbio/base/traverse"
	"github.com/grailbio/infra"
	"github.com/grailbio/reflow"
	"github.com/grailbio/reflow/assoc/dydbassoc"
	"github.com/grailbio/reflow/errors"
	infra2 "github.com/grailbio/reflow/infra"
	"github.com/grailbio/reflow/pool"
	"github.com/grailbio/reflow/taskdb"
)

func init() {
	infra.Register("dynamodbtask", new(TaskDB))
}

var (
	dateKeepaliveIndex = "Date-Keepalive-index"
	idIndex            = "ID-index"
	id4Index           = "ID4-ID-index"
	runIDIndex         = "RunID-index"
)

type objType string

const (
	run  objType = "run"
	task objType = "task"
)

const (
	// TimeLayout is the time layout used to serialize time to dynamodb attributes.
	timeLayout = time.RFC3339
	// DateLayout is the layout used to serialize date.
	dateLayout = "2006-01-02"

	// Default provisioned capacities for DynamoDB.
	writecap = 10
	readcap  = 20
)

// Column names used in dynamodb table
const (
	colID        = "ID"
	colID4       = "ID4"
	colRunID     = "RunID"
	colRunID4    = "RunID4"
	colFlowID    = "FlowID"
	colResultID  = "ResultID"
	colKeepalive = "Keepalive"
	colStartTime = "StartTime"
	colStdout    = "Stdout"
	colStderr    = "Stderr"
	colInspect   = "Inspect"
	colURI       = "URI"
	colLabels    = "Labels"
	colUser      = "User"
	colType      = "Type"
	colDate      = "Date"
)

// TaskDB implements the dynamodb backed taskdb.TaskDB interface to
// store run/task state and metadata.
// Each association is either:
// a) RunID and its associated metadata (run labels, user info, and leases)
// b) TaskID and its associated metadata (RunID that spawned this task, FlowID of the node, and leases)
type TaskDB struct {
	// DB is the dynamodb.
	DB dynamodbiface.DynamoDBAPI
	// TableName is the table to write the run/task info to.
	TableName string
	// Labels on the run.
	Labels []string
	// User who initiated this run.
	User string
	// Limiter limits number of concurrent operations.
	limiter *limiter.Limiter
}

// Help implements infra.Provider
func (TaskDB) Help() string {
	return "configure a dynamodb table to store run/task information"
}

// Init implements infra.Provider
func (t *TaskDB) Init(sess *session.Session, assoc *dydbassoc.Assoc, user *infra2.User, labels pool.Labels) error {
	t.limiter = limiter.New()
	t.limiter.Release(32)
	t.DB = dynamodb.New(sess)
	t.Labels = make([]string, 0, len(labels))
	for k, v := range labels {
		t.Labels = append(t.Labels, fmt.Sprintf("%s=%s", k, v))
	}
	t.User = string(*user)
	t.TableName = assoc.TableName
	return nil
}

// CreateRun sets a new run in the taskdb with the given id, labels and user.
func (t *TaskDB) CreateRun(ctx context.Context, id digest.Digest, user string) error {
	input := &dynamodb.PutItemInput{
		TableName: aws.String(t.TableName),
		Item: map[string]*dynamodb.AttributeValue{
			colID: {
				S: aws.String(id.String()),
			},
			colID4: {
				S: aws.String(id.HexN(4)),
			},
			colLabels: {
				SS: aws.StringSlice(t.Labels),
			},
			colUser: {
				S: aws.String(user),
			},
			colType: {
				S: aws.String(string(run)),
			},
			colStartTime: {
				S: aws.String(time.Now().UTC().Format(timeLayout)),
			},
		},
	}
	_, err := t.DB.PutItemWithContext(ctx, input)
	return err
}

// CreateTask sets a new task in the taskdb with the given taskid, runid and flowid.
func (t *TaskDB) CreateTask(ctx context.Context, id, runid, flowid digest.Digest, uri string) error {
	input := &dynamodb.PutItemInput{
		TableName: aws.String(t.TableName),
		Item: map[string]*dynamodb.AttributeValue{
			colID: {
				S: aws.String(id.String()),
			},
			colID4: {
				S: aws.String(id.HexN(4)),
			},
			colRunID: {
				S: aws.String(runid.String()),
			},
			colRunID4: {
				S: aws.String(runid.HexN(4)),
			},
			colFlowID: {
				S: aws.String(flowid.String()),
			},
			colType: {
				S: aws.String(string(task)),
			},
			colStartTime: {
				S: aws.String(time.Now().UTC().Format(timeLayout)),
			},
			colURI: {
				S: aws.String(uri),
			},
			colLabels: {
				SS: aws.StringSlice(t.Labels),
			},
		},
	}
	_, err := t.DB.PutItemWithContext(ctx, input)
	return err
}

// SetTaskResult sets the task result id.
func (t *TaskDB) SetTaskResult(ctx context.Context, id, result digest.Digest) error {
	input := &dynamodb.UpdateItemInput{
		TableName: aws.String(t.TableName),
		Key: map[string]*dynamodb.AttributeValue{
			colID: {
				S: aws.String(id.String()),
			},
		},
		UpdateExpression: aws.String(fmt.Sprintf("SET %s = :result", colResultID)),
		ExpressionAttributeValues: map[string]*dynamodb.AttributeValue{
			":result": {S: aws.String(result.String())},
		},
	}
	_, err := t.DB.UpdateItemWithContext(ctx, input)
	return err
}

// SetTaskAttrs sets the stdout, stderr and inspect ids for the task.
func (t *TaskDB) SetTaskAttrs(ctx context.Context, id, stdout, stderr, inspect digest.Digest) error {
	input := &dynamodb.UpdateItemInput{
		TableName: aws.String(t.TableName),
		Key: map[string]*dynamodb.AttributeValue{
			colID: {
				S: aws.String(id.String()),
			},
		},
		UpdateExpression: aws.String(fmt.Sprintf("SET %s = :stdout, %s = :stderr, %s = :inspect", colStdout, colStderr, colInspect)),
		ExpressionAttributeValues: map[string]*dynamodb.AttributeValue{
			":stdout":  {S: aws.String(stdout.String())},
			":stderr":  {S: aws.String(stderr.String())},
			":inspect": {S: aws.String(inspect.String())},
		},
	}
	_, err := t.DB.UpdateItemWithContext(ctx, input)
	return err
}

func date(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func dates(beg, end time.Time) (dates []time.Time) {
	for beg, end = date(beg), date(end); !end.Before(beg); beg = beg.AddDate(0, 0, 1) {
		dates = append(dates, beg)
	}
	return
}

// Keepalive sets the keepalive for the specified testId (run/task) to keepalive.
func (t *TaskDB) Keepalive(ctx context.Context, id digest.Digest, keepalive time.Time) error {
	keepalive = keepalive.UTC()
	input := &dynamodb.UpdateItemInput{
		TableName: aws.String(t.TableName),
		Key: map[string]*dynamodb.AttributeValue{
			colID: {
				S: aws.String(id.String()),
			},
		},
		UpdateExpression: aws.String(fmt.Sprintf("SET %s = :ka, #Date = :date", colKeepalive)),
		ExpressionAttributeValues: map[string]*dynamodb.AttributeValue{
			":ka":   {S: aws.String(keepalive.Format(timeLayout))},
			":date": {S: aws.String(keepalive.Format(dateLayout))},
		},
		ExpressionAttributeNames: map[string]*string{
			"#Date": aws.String(colDate),
		},
	}
	_, err := t.DB.UpdateItemWithContext(ctx, input)
	return err
}

func (t *TaskDB) buildRunIdQuery(q taskdb.Query) []*dynamodb.QueryInput {
	const keyExpression = colRunID + " = :rid"
	attributeValues := make(map[string]*dynamodb.AttributeValue)
	attributeValues[":rid"] = &dynamodb.AttributeValue{S: aws.String(q.RunID.String())}
	input := &dynamodb.QueryInput{
		TableName:                 aws.String(t.TableName),
		IndexName:                 aws.String(runIDIndex),
		KeyConditionExpression:    aws.String(keyExpression),
		ExpressionAttributeValues: attributeValues,
	}
	return []*dynamodb.QueryInput{input}
}

func (t *TaskDB) buildIdQuery(q taskdb.Query, typ objType) []*dynamodb.QueryInput {
	var (
		keyExpression   string
		attributeValues = make(map[string]*dynamodb.AttributeValue)
	)
	index := idIndex
	if q.ID.IsAbbrev() {
		keyExpression = fmt.Sprintf("%v = :id4", colID4)
		attributeValues[":id4"] = &dynamodb.AttributeValue{S: aws.String(q.ID.HexN(4))}
		index = id4Index
	} else {
		keyExpression = fmt.Sprintf("%s = :testId", colID)
		attributeValues[":testId"] = &dynamodb.AttributeValue{S: aws.String(q.ID.String())}
	}
	const filterExpression = "#Type = :type"
	attributeValues[":type"] = &dynamodb.AttributeValue{S: aws.String(string(typ))}
	input := &dynamodb.QueryInput{
		TableName:                 aws.String(t.TableName),
		IndexName:                 aws.String(index),
		KeyConditionExpression:    aws.String(keyExpression),
		ExpressionAttributeValues: attributeValues,
		FilterExpression:          aws.String(filterExpression),
		ExpressionAttributeNames: map[string]*string{
			"#Type": aws.String(colType),
		},
	}
	return []*dynamodb.QueryInput{input}
}

func (t *TaskDB) buildQueries(q taskdb.Query, typ objType) []*dynamodb.QueryInput {
	if !q.ID.IsZero() {
		return t.buildIdQuery(q, typ)
	}
	if !q.RunID.IsZero() && typ == run {
		panic(fmt.Sprintf("taskdb invalid query: %v", q))
	}
	// Build time bucket based queries.
	type part struct {
		keyExpression string
		attrValues    map[string]*dynamodb.AttributeValue
		attrNames     map[string]*string
	}
	var (
		keyExpression    string
		timeBuckets      []part
		attributeValues  = make(map[string]*dynamodb.AttributeValue)
		attributeNames   = make(map[string]*string)
		filterExpression []string
		now              = time.Now().UTC()
	)
	if q.Since.IsZero() {
		panic("taskdb invalid query: missing since")
	}
	since := q.Since.UTC()
	for _, d := range dates(since, now) {
		part := part{
			keyExpression: "#Date = :date and " + colKeepalive + " > :ka ",
			attrValues: map[string]*dynamodb.AttributeValue{
				":date": &dynamodb.AttributeValue{S: aws.String(d.Format(dateLayout))},
				":ka":   &dynamodb.AttributeValue{S: aws.String(since.Format(timeLayout))},
			},
			attrNames: map[string]*string{
				"#Date": aws.String(colDate),
			},
		}
		timeBuckets = append(timeBuckets, part)
	}

	if q.User != "" {
		filterExpression = append(filterExpression, "#User = :user")
		attributeValues[":user"] = &dynamodb.AttributeValue{S: aws.String(q.User)}
		attributeNames["#User"] = aws.String(colUser)
	}
	if typ == run {
		filterExpression = append(filterExpression, "#Type = :type")
		attributeValues[":type"] = &dynamodb.AttributeValue{S: aws.String(string(typ))}
		attributeNames["#Type"] = aws.String(colType)
	}
	if len(timeBuckets) > 0 {
		var queries []*dynamodb.QueryInput
		for _, ti := range timeBuckets {
			for k, v := range attributeValues {
				ti.attrValues[k] = v
			}
			for k, v := range attributeNames {
				ti.attrNames[k] = v
			}
			query := &dynamodb.QueryInput{
				TableName:                 aws.String(t.TableName),
				IndexName:                 aws.String(dateKeepaliveIndex),
				KeyConditionExpression:    aws.String(ti.keyExpression),
				ExpressionAttributeValues: ti.attrValues,
			}
			if len(ti.attrNames) > 0 {
				query.ExpressionAttributeNames = ti.attrNames
			}
			if len(filterExpression) > 0 {
				query.FilterExpression = aws.String(strings.Join(filterExpression, " and "))
			}
			queries = append(queries, query)
		}
		return queries
	}

	input := &dynamodb.QueryInput{
		TableName:                 aws.String(t.TableName),
		IndexName:                 aws.String(dateKeepaliveIndex),
		KeyConditionExpression:    aws.String(keyExpression),
		ExpressionAttributeValues: attributeValues,
	}
	if len(attributeNames) > 0 {
		input.ExpressionAttributeNames = attributeNames
	}
	if len(filterExpression) > 0 {
		input.FilterExpression = aws.String(strings.Join(filterExpression, " and "))
	}
	return []*dynamodb.QueryInput{input}
}

// Tasks returns tasks that matches the query.
func (t *TaskDB) Tasks(ctx context.Context, query taskdb.Query) ([]taskdb.Task, error) {
	var queries []*dynamodb.QueryInput
	if !query.RunID.IsZero() {
		queries = t.buildRunIdQuery(query)
	} else {
		queries = t.buildQueries(query, task)
	}
	var (
		count     uint64
		responses = make([]*dynamodb.QueryOutput, len(queries))
		err       error
		errs      []error
	)
	err = traverse.Each(len(queries), func(i int) error {
		for _, query := range queries {
			resp, err := t.DB.QueryWithContext(ctx, query)
			if err != nil {
				if aerr, ok := err.(awserr.Error); ok {
					switch aerr.Code() {
					case "ValidationException":
						if strings.Contains(aerr.Message(),
							"The table does not have the specified index") {
							return errors.E(`index missing: run "reflow migrate"`, err)
						}
					}
				}
				return err
			}
			atomic.AddUint64(&count, uint64(len(resp.Items)))
			responses[i] = resp
		}
		return nil
	})
	if err != nil {
		return []taskdb.Task{}, err
	}
	tasks := make([]taskdb.Task, 0, count)
	for i := range responses {
		if responses[i] == nil {
			continue
		}
		for _, it := range responses[i].Items {
			var id, fid, runid, result, stderr, stdout, inspect digest.Digest

			id, err = digest.Parse(*it[colID].S)
			if err != nil {
				errs = append(errs, fmt.Errorf("parse id %v: %v", *it[colID], err))
			}
			if !query.ID.IsZero() && query.ID.IsAbbrev() {
				if !id.Expands(query.ID) {
					continue
				}
			}
			fid, err := reflow.Digester.Parse(*it[colFlowID].S)
			if err != nil {
				errs = append(errs, fmt.Errorf("parse flowid %v: %v", *it[colFlowID].S, err))
			}
			runid, err = digest.Parse(*it[colRunID].S)
			if err != nil {
				errs = append(errs, fmt.Errorf("parse runid %v: %v", *it[colRunID].S, err))
			}
			if resultID, ok := it[colResultID]; ok {
				result, err = digest.Parse(*resultID.S)
				if err != nil {
					errs = append(errs, fmt.Errorf("parse resultid %v: %v", *resultID.S, err))
				}
			}
			ka, err := time.Parse(timeLayout, *it[colKeepalive].S)
			if err != nil {
				errs = append(errs, fmt.Errorf("parse keepalive %v: %v", *it[colKeepalive].S, err))
			}
			st, err := time.Parse(timeLayout, *it[colStartTime].S)
			if err != nil {
				errs = append(errs, fmt.Errorf("parse starttime %v: %v", *it[colStartTime].S, err))
			}
			if v, ok := it[colStdout]; ok {
				stdout, err = digest.Parse(*v.S)
				if err != nil {
					errs = append(errs, fmt.Errorf("parse stdout %v: %v", *it[colStdout].S, err))
				}
			}
			if v, ok := it[colStderr]; ok {
				stderr, err = digest.Parse(*v.S)
				if err != nil {
					errs = append(errs, fmt.Errorf("parse stderr %v: %v", *it[colStderr].S, err))
				}
			}
			if v, ok := it[colInspect]; ok {
				inspect, err = digest.Parse(*v.S)
				if err != nil {
					errs = append(errs, fmt.Errorf("parse inspect %v: %v", *it[colInspect].S, err))
				}
			}
			uri := *it[colURI].S
			tasks = append(tasks, taskdb.Task{
				ID:        id,
				RunID:     runid,
				FlowID:    fid,
				ResultID:  result,
				URI:       uri,
				Keepalive: ka,
				Start:     st,
				Stdout:    stdout,
				Stderr:    stderr,
				Inspect:   inspect,
			})
		}
	}
	if len(errs) == 0 {
		return tasks, nil
	}
	var b strings.Builder
	for i, err := range errs {
		b.WriteString(err.Error())
		if i != len(errs)-1 {
			b.WriteString(", ")
		}
	}
	return []taskdb.Task{}, errors.New(b.String())
}

// Runs returns runs that matches the query.
func (t *TaskDB) Runs(ctx context.Context, query taskdb.Query) ([]taskdb.Run, error) {
	queries := t.buildQueries(query, run)
	var (
		count     uint64
		responses = make([]*dynamodb.QueryOutput, len(queries))
		errs      []error
		err       error
	)
	err = traverse.Each(len(queries), func(i int) error {
		resp, err := t.DB.QueryWithContext(ctx, queries[i])
		if err != nil {
			if aerr, ok := err.(awserr.Error); ok {
				switch aerr.Code() {
				case "ValidationException":
					if strings.Contains(aerr.Message(),
						"The table does not have the specified index") {
						return errors.E(`index missing: run "reflow migrate"`, err)
					}
				}
			}
			return err
		}
		atomic.AddUint64(&count, uint64(len(resp.Items)))
		responses[i] = resp
		return nil
	})
	if err != nil {
		return []taskdb.Run{}, err
	}
	runs := make([]taskdb.Run, 0, count)
	for i := range responses {
		if responses[i] == nil {
			continue
		}
		for _, it := range responses[i].Items {
			id, err := reflow.Digester.Parse(*it[colID].S)
			if err != nil {
				errs = append(errs, fmt.Errorf("parse id %v: %v", *it[colID].S, err))
			}
			if !query.ID.IsZero() && query.ID.IsAbbrev() {
				if !id.Expands(query.ID) {
					continue
				}
			}
			l := make(pool.Labels)
			for _, va := range it[colLabels].SS {
				vals := strings.Split(*va, "=")
				if len(vals) != 2 {
					errs = append(errs, fmt.Errorf("label not well formed: %v", *va))
					continue
				}
				l[vals[0]] = vals[1]
			}
			ka, err := time.Parse(timeLayout, *it[colKeepalive].S)
			if err != nil {
				errs = append(errs, fmt.Errorf("parse keepalive %v: %v", *it[colKeepalive].S, err))
			}
			st, err := time.Parse(timeLayout, *it[colStartTime].S)
			if err != nil {
				errs = append(errs, fmt.Errorf("parse starttime %v: %v", *it[colStartTime].S, err))
			}
			runs = append(runs, taskdb.Run{
				ID:        id,
				Labels:    l,
				User:      *it["User"].S,
				Keepalive: ka,
				Start:     st})
		}
	}
	if len(errs) == 0 {
		return runs, nil
	}
	var b strings.Builder
	for i, err := range errs {
		b.WriteString(err.Error())
		if i != len(errs)-1 {
			b.WriteString(", ")
		}
	}
	return []taskdb.Run{}, fmt.Errorf("%s", b.String())
}
