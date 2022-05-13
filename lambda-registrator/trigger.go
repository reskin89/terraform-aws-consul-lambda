package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	sdkARN "github.com/aws/aws-sdk-go/aws/arn"
	"github.com/aws/aws-sdk-go/service/lambda"

	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/go-multierror"
)

const (
	prefix                = "serverless.consul.hashicorp.com/v1alpha1/lambda"
	enabledTag            = prefix + "/enabled"
	arnTag                = prefix + "/arn"
	payloadPassthroughTag = prefix + "/payload-passhthrough"
	regionTag             = prefix + "/region"
	partitionTag          = prefix + "/partition"
	namespaceTag          = prefix + "/namespace"
	aliasesTag            = prefix + "/aliases"
	invocationModeTag     = prefix + "/invocation-mode"
)

const (
	asynchronousInvocationMode = "ASYNCHRONOUS"
	synchronousInvocationMode  = "SYNCHRONOUS"
)

var (
	regionUndefinedErr = errors.New("region isn't populated")
	arnUndefinedErr    = errors.New("arn isn't populated")
	notEnterpriseErr   = errors.New("namespaces and admin partitions can't be used with open source Consul")
)

type AWSEvent struct {
	Detail Detail `json:"detail"`
}

type Detail struct {
	EventName         string            `json:"eventName"`
	ResponseElements  ResponseElements  `json:"responseElements"`
	RequestParameters RequestParameters `json:"requestParameters"`
}

type ResponseElements struct {
	FunctionArn string `json:"functionArn"`
}

type RequestParameters struct {
	FunctionName string `json:"functionName"`
	Resource     string `json:"resource"`
}

type EnterpriseMeta struct {
	Namespace string
	Partition string
}

func (e Environment) AWSEventToEvents(event AWSEvent) ([]Event, error) {
	var events []Event
	var arn string
	switch event.Detail.EventName {
	case "CreateFunction20150331", "CreateFunction":
		arn = event.Detail.ResponseElements.FunctionArn
	case "TagResource20170331v2", "TagResource20170331", "TagResource",
		"UntagResource20170331v2", "UntagResource20170331", "UntagResource":
		arn = event.Detail.RequestParameters.Resource
	default:
		return events, fmt.Errorf("unsupported event kind %s", event.Detail.EventName)
	}

	if arn == "" {
		return events, arnUndefinedErr
	}

	upsertEvents, err := e.GetLambdaData(arn)

	if err != nil {
		return events, err
	}

	for _, e := range upsertEvents {
		events = append(events, e)
	}

	return events, nil
}

const (
	// `,` isn't allowed
	// https://docs.aws.amazon.com/directoryservice/latest/devguide/API_Tag.html
	listSeparator = "+"
)

func (e Environment) GetLambdaData(arn string) ([]Event, error) {
	createService := false
	payloadPassthrough := false
	invocationMode := synchronousInvocationMode
	var aliases []string

	// This is terrible, but it saves tons of API calls to GetFunction just for
	// the function name.
	parsedARN, err := sdkARN.Parse(arn)
	if err != nil {
		return nil, err
	}
	functionName := ""
	if i := strings.IndexByte(parsedARN.Resource, ':'); i != -1 {
		functionName = parsedARN.Resource[i+1:]
	}

	tagOutput, err := e.Lambda.ListTags(&lambda.ListTagsInput{
		Resource: &arn,
	})

	if err != nil {
		return nil, err
	}

	tags := tagOutput.Tags

	if tags[enabledTag] != nil {
		createService = *tags[enabledTag] == "true"
	}

	if tags[payloadPassthroughTag] != nil {
		payloadPassthrough = *tags[payloadPassthroughTag] == "true"
	}

	if tags[invocationModeTag] != nil {
		invocationMode = *tags[invocationModeTag]
		switch invocationMode {
		case asynchronousInvocationMode, synchronousInvocationMode:
		default:
			return nil, fmt.Errorf("invalid invocation mode: %s", invocationMode)
		}
	}

	var em *EnterpriseMeta
	if tags[namespaceTag] != nil {
		em = &EnterpriseMeta{Namespace: *tags[namespaceTag], Partition: "default"}
	}

	if tags[partitionTag] != nil {
		partition := *tags[partitionTag]
		if em == nil {
			em = &EnterpriseMeta{Namespace: "default", Partition: partition}
		} else {
			em.Partition = partition
		}
	}

	if !e.IsEnterprise && em != nil {
		return nil, notEnterpriseErr
	}

	// Ignore events in unhandled partitions.
	if e.IsEnterprise && em != nil {
		if _, ok := e.Partitions[em.Partition]; !ok {
			return nil, nil
		}
	}

	if aliasesRaw, ok := tags[aliasesTag]; ok {
		aliases = strings.Split(*aliasesRaw, listSeparator)
	}

	var events []Event

	if createService {
		baseUpsertEvent := UpsertEvent{
			PayloadPassthrough: payloadPassthrough,
			ServiceName:        functionName,
			ARN:                arn,
			EnterpriseMeta:     em,
			InvocationMode:     invocationMode,
		}

		events = append(events, baseUpsertEvent)

		for _, aliasName := range aliases {
			e := baseUpsertEvent.AddAlias(aliasName)
			events = append(events, e)
		}
	} else {
		baseDeleteEvent := DeleteEvent{
			ServiceName:    functionName,
			EnterpriseMeta: em,
		}

		events = append(events, baseDeleteEvent)

		for _, aliasName := range aliases {
			e := baseDeleteEvent.AddAlias(aliasName)
			events = append(events, e)
		}
	}

	return events, nil
}

func (e Environment) FullSyncData() ([]Event, error) {
	lambdas, err := e.getLambdas()
	if err != nil {
		return nil, err
	}

	enterpriseMetas, err := e.getEnterpriseMetas()
	if err != nil {
		return nil, err
	}

	// EnterpriseMeta is nil for OSS Consul.
	consulServices, err := e.getConsulServices(enterpriseMetas)
	if err != nil {
		return nil, err
	}

	events := e.constructUpsertEvents(lambdas, consulServices)
	return append(events, e.constructDeleteEvents(lambdas, consulServices)...), nil
}

// getLambdas makes requests to the AWS APIs to get data about every Lambda and
// constructs events to register that Lambdas into Consul.
func (e Environment) getLambdas() (map[*EnterpriseMeta]map[string]Event, error) {
	var resultErr error
	maxItems := 50
	input := &lambda.ListFunctionsInput{MaxItems: aws.Int64(int64(maxItems))}
	lambdas := make(map[*EnterpriseMeta]map[string]Event)

	err := e.Lambda.ListFunctionsPages(input, func(output *lambda.ListFunctionsOutput, lastPage bool) bool {
		for _, fn := range output.Functions {
			events, err := e.GetLambdaData(*fn.FunctionArn)
			if err != nil {
				resultErr = multierror.Append(resultErr, err)
				return true
			}

			for _, event := range events {
				switch e := event.(type) {
				case UpsertEvent:
					if lambdas[e.EnterpriseMeta] == nil {
						lambdas[e.EnterpriseMeta] = make(map[string]Event)
					}

					lambdas[e.EnterpriseMeta][e.ServiceName] = event
				case DeleteEvent:
					if lambdas[e.EnterpriseMeta] == nil {
						lambdas[e.EnterpriseMeta] = make(map[string]Event)
					}

					lambdas[e.EnterpriseMeta][e.ServiceName] = event
				}
			}
		}

		return true
	})

	if err != nil {
		resultErr = multierror.Append(resultErr, err)
	}

	return lambdas, resultErr
}

// getEnterpriseMetas determines which Consul partitions will be synced. A nil return
// value is used for OSS Consul.
func (e Environment) getEnterpriseMetas() ([]*EnterpriseMeta, error) {
	var enterpriseMetas []*EnterpriseMeta
	if e.IsEnterprise {
		for partition := range e.Partitions {
			namespaces, _, err := e.ConsulClient.Namespaces().List(&api.QueryOptions{Partition: partition})
			if err != nil {
				return nil, err
			}

			for _, namespace := range namespaces {
				enterpriseMetas = append(enterpriseMetas, &EnterpriseMeta{
					Partition: partition,
					Namespace: namespace.Name,
				})
			}
		}
	} else {
		enterpriseMetas = append(enterpriseMetas, nil)
	}

	return enterpriseMetas, nil
}

// getConsulServices retrieves all Consul services that are managed by Lambda registrator.
func (e Environment) getConsulServices(enterpriseMetas []*EnterpriseMeta) (map[*EnterpriseMeta]map[string]struct{}, error) {
	consulServices := make(map[*EnterpriseMeta]map[string]struct{})
	// Fetching Consul serices.
	for _, em := range enterpriseMetas {
		var queryOptions *api.QueryOptions
		if em != nil {
			queryOptions = &api.QueryOptions{
				Partition: em.Partition,
				Namespace: em.Namespace,
			}
		}
		services, _, err := e.ConsulClient.Catalog().Services(queryOptions)
		if err != nil {
			return nil, err
		}
		consulServices[em] = make(map[string]struct{})
		for serviceName, tags := range services {
			for _, t := range tags {
				if managedLambdaTag == t {
					consulServices[em][serviceName] = struct{}{}
					break
				}
			}
		}
	}

	return consulServices, nil
}

// constructUpsertEvents determines which upsert events need to be processed to
// synchronize Consul with Lambda.
func (e Environment) constructUpsertEvents(lambdas map[*EnterpriseMeta]map[string]Event, consulServices map[*EnterpriseMeta]map[string]struct{}) []Event {
	var events []Event

	for enterpriseMeta, lambdaEvents := range lambdas {
		for serviceName, event := range lambdaEvents {
			switch e := event.(type) {
			case UpsertEvent:
				if consulEvents, ok := consulServices[enterpriseMeta]; ok {
					if _, ok := consulEvents[serviceName]; !ok {
						events = append(events, e)
					}
				} else {
					events = append(events, e)
				}
			case DeleteEvent:
				if consulEvents, ok := consulServices[enterpriseMeta]; ok {
					if _, ok := consulEvents[serviceName]; ok {
						events = append(events, e)
					}
				}
			}
		}
	}

	return events
}

// constructUpsertEvents determines which delete events need to be processed to
// synchronize Consul with Lambda.
func (e Environment) constructDeleteEvents(lambdas map[*EnterpriseMeta]map[string]Event, consulServices map[*EnterpriseMeta]map[string]struct{}) []Event {
	var events []Event
	// Constructing delete events for services that need to be deregistered in Consul
	for enterpriseMeta, consulService := range consulServices {
		for serviceName := range consulService {
			deleteEvent := DeleteEvent{ServiceName: serviceName, EnterpriseMeta: enterpriseMeta}
			if lambdaEvents, ok := lambdas[enterpriseMeta]; ok {
				if _, ok := lambdaEvents[serviceName]; !ok {
					events = append(events, deleteEvent)
				}
			} else {
				events = append(events, deleteEvent)
			}
		}
	}

	return events
}
