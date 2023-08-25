package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"

	"cloud.google.com/go/pubsub"
	"github.com/linkedin/goavro"
	"github.com/pkg/errors"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
)

type CodecRecord = map[string]interface{}
type FBRecord = map[interface{}]interface{}

type Keeper interface {
	Send(ctx context.Context, data []byte) *pubsub.PublishResult
	Stop()
	ConvertAvroNative(record FBRecord) ([]byte, error)
}

type GooglePubSub struct {
	client    *pubsub.Client
	topic     *pubsub.Topic
	codec     func(record CodecRecord) ([]byte, error)
	schemaMap map[string]AvroDataType
}

type AvroDataType struct {
	Type     string
	Nullable bool
}

type AvroSchemaMap = map[string]AvroDataType

type AvroField struct {
	Name    string      `json:"name"`
	Type    interface{} `json:"type"`
	Default interface{} `json:"default"`
}

type AvroSchema struct {
	Fields []AvroField `json:"fields"`
	Name   string      `json:"name"`
	Type   string      `json:"type"`
}

func NewKeeper(projectId string, topicName string, secret *Secret,
	publishSetting *pubsub.PublishSettings,
	schemaConfig *pubsub.SchemaConfig,
) (Keeper, error) {
	if projectId == "" || topicName == "" || secret == nil {
		return nil, fmt.Errorf("[err] NewKeeper empty params")
	}

	var keyBytes []byte
	if secret.jwt == "" && secret.jwtPath == "" {
		return nil, fmt.Errorf("[err] Neither jwtString nor jwtPath")
	} else if secret.jwt != "" {
		keyBytes = []byte(secret.jwt)
	} else if secret.jwtPath != "" {
		fileBytes, err := os.ReadFile(secret.jwtPath)
		if err != nil {
			return nil, errors.Wrap(err, "[err] jwt path")
		}
		keyBytes = fileBytes
	}

	config, err := google.JWTConfigFromJSON(keyBytes, pubsub.ScopePubSub)
	if err != nil {
		return nil, errors.Wrap(err, "[err] jwt config")
	}

	ctx := context.Background()
	client, err := pubsub.NewClient(ctx, projectId, option.WithTokenSource(config.TokenSource(ctx)))
	if err != nil {
		return nil, errors.Wrap(err, "[err] pubsub client")
	}

	topic := client.Topic(topicName)
	if publishSetting != nil {
		topic.PublishSettings = *publishSetting
	} else {
		topic.PublishSettings = pubsub.DefaultPublishSettings
	}

	cfg, err := topic.Config(ctx)
	if err != nil {
		return nil, fmt.Errorf("topic.Config err: %v", err)
	}
	encoding := cfg.SchemaSettings.Encoding

	var codec func(record CodecRecord) ([]byte, error)
	var pubs *GooglePubSub
	switch schemaType {
	case pubsub.SchemaAvro:
		var avroSchema AvroSchema
		if err := json.Unmarshal([]byte(schemaConfig.Definition), &avroSchema); err != nil {
			return nil, fmt.Errorf("Avro Schema Unmarshal: %v", err)
		}
		schemaMap := makeSchemaMap(avroSchema)

		avroCodec, err := goavro.NewCodec(schemaConfig.Definition)
		if err != nil {
			return nil, fmt.Errorf("Avro NewCodec: %v", err)
		}

		switch encoding {
		case pubsub.EncodingBinary:
			codec = func(record CodecRecord) ([]byte, error) { return avroCodec.BinaryFromNative(nil, record) }
		case pubsub.EncodingJSON:
			codec = func(record CodecRecord) ([]byte, error) { return avroCodec.TextualFromNative(nil, record) }
		default:
			return nil, fmt.Errorf("invalid encoding: %v", encoding)
		}
		pubs = &GooglePubSub{client, topic, codec, schemaMap}
	// case pubsub.SchemaProtocolBuffer: [TODO]
	//    switch encoding {
	//    case pubsub.EncodingBinary:
	//        codec = func(record CodecRecord) ([]byte, error) {return ####}
	//    case pubsub.EncodingJSON:
	//        codec = func(record CodecRecord) ([]byte, error) {return ####}
	//    default;
	//        return nil, fmt.Errorf("invalid encoding: %v", encoding)
	//    pubs = &GooglePubSub{client, topic, codec}
	// }
	default:
		pubs = &GooglePubSub{client, topic, nil, nil}
	}
	return Keeper(pubs), nil
}

func (gps *GooglePubSub) Send(ctx context.Context, data []byte) *pubsub.PublishResult {
	if len(data) == 0 {
		return nil
	}
	return gps.topic.Publish(ctx, &pubsub.Message{Data: data})
}

func (gps *GooglePubSub) Stop() {
	gps.topic.Stop()
}

func (gps *GooglePubSub) ConvertAvroNative(fbr FBRecord) ([]byte, error) {
	cr := make(CodecRecord)
	for k, v := range fbr {
		strKey := fmt.Sprintf("%v", k)
		avroType, ok := gps.schemaMap[strKey]
		if !ok {
			continue
		}
		cr[strKey] = convert(avroType, v)
	}

	a, err := gps.codec(cr)
	return a, err
}

func makeSchemaMap(as AvroSchema) AvroSchemaMap {
	var m = make(AvroSchemaMap)
	for _, f := range as.Fields {
		rv := reflect.ValueOf(f.Type)
		switch rv.Kind() {
		case reflect.String:
			m[f.Name] = AvroDataType{Type: rv.String(), Nullable: false}
		case reflect.Slice:
			nullableType := rv.Index(1)
			switch nullableType.Kind() {
			case reflect.String:
				m[f.Name] = AvroDataType{Type: nullableType.String(), Nullable: true}
			case reflect.Interface:
				m[f.Name] = AvroDataType{Type: nullableType.Interface().(string), Nullable: true}
			}
		}
	}
	return m
}

func convert(at AvroDataType, v interface{}) interface{} {
	var x interface{}

	switch at.Type {
	case "boolean":
		x = v.(bool)
	case "int":
		switch v.(type) {
		case uint64:
			x = int32(v.(uint64))
		case int64:
			x = int32(v.(int64))
		}
	case "long":
		switch v.(type) {
		case uint64:
			x = v.(uint64)
		case int64:
			x = v.(int64)
		}
	case "float":
		x = float32(v.(float64))
	case "double":
		x = v.(float64)
	case "bytes":
		x = v.([]byte)
	case "string":
		x = string(v.([]byte))
	default:
		x = v
	}

	if at.Nullable {
		return goavro.Union(at.Type, x)
	} else {
		return x
	}
}
