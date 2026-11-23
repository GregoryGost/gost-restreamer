package youtube

import (
	_ "embed"
	"sync"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

const (
	// streamServiceName — полное имя сервиса в опубликованной схеме YouTube.
	streamServiceName = "youtube.api.v3.V3DataLiveChatMessageService"
	// streamMethodName — имя RPC, которое также используется в пути HTTP/2.
	streamMethodName = "StreamList"
	// streamRPCPath адресует серверный поток в gRPC.
	streamRPCPath = "/" + streamServiceName + "/" + streamMethodName
)

// streamDescriptorSet содержит опубликованную схему StreamList и её импорты.
//
//go:embed stream_list.protoset
var streamDescriptorSet []byte

// loadStreamSchema разбирает встроенную схему один раз и делит неизменяемый
// дескриптор метода между попытками чтения. Ошибка схемы остаётся локальной.
var loadStreamSchema = sync.OnceValues(parseStreamSchema)

// parseStreamSchema находит метод и проверяет форму RPC перед созданием
// запросов и ответов из его дескрипторов.
func parseStreamSchema() (protoreflect.MethodDescriptor, error) {
	// set содержит файлы схемы вместе с транзитивными импортами.
	var set descriptorpb.FileDescriptorSet
	// err сообщает, удалось ли декодировать встроенный набор.
	if err := proto.Unmarshal(streamDescriptorSet, &set); err != nil {
		return nil, core.ErrInvalid
	}
	// files связывает типы в файлах схемы. err сообщает о неразрешимой ссылке.
	files, err := protodesc.NewFiles(&set)
	if err != nil {
		return nil, core.ErrInvalid
	}
	// descriptor содержит объявление сервиса или ошибку его поиска в err.
	descriptor, err := files.FindDescriptorByName(streamServiceName)
	if err != nil {
		return nil, core.ErrInvalid
	}
	// service предоставляет каталог методов. ok подтверждает вид объявления.
	service, ok := descriptor.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, core.ErrInvalid
	}
	// method задаёт типы запроса и потока ответов для gRPC.
	method := service.Methods().ByName(streamMethodName)
	if method == nil || method.IsStreamingClient() || !method.IsStreamingServer() {
		return nil, core.ErrInvalid
	}
	return method, nil
}

// buildStreamRequest указывает фактический чат chatID и запрашивает данные,
// необходимые для нормализации. resume добавляется только при продолжении.
func buildStreamRequest(method protoreflect.MethodDescriptor, chatID, resume string) *dynamicpb.Message {
	// request — один protobuf-запрос текущей физической попытки.
	request := dynamicpb.NewMessage(method.Input())
	// fields разрешает имена полей по опубликованной схеме.
	fields := method.Input().Fields()
	request.Set(fields.ByName("live_chat_id"), protoreflect.ValueOfString(chatID))
	if resume != "" {
		request.Set(fields.ByName("page_token"), protoreflect.ValueOfString(resume))
	}
	// parts содержит список возвращаемых частей ресурса.
	parts := request.Mutable(fields.ByName("part")).List()
	parts.Append(protoreflect.ValueOfString("id"))
	parts.Append(protoreflect.ValueOfString("snippet"))
	parts.Append(protoreflect.ValueOfString("authorDetails"))
	return request
}

// protobufField разрешает известное поле name в дескрипторе message.
func protobufField(message protoreflect.Message, name protoreflect.Name) protoreflect.FieldDescriptor {
	return message.Descriptor().Fields().ByName(name)
}

// protobufString возвращает строку name из message. Пустая строка также
// представляет отсутствие необязательного поля в схеме proto2.
func protobufString(message protoreflect.Message, name protoreflect.Name) string {
	return message.Get(protobufField(message, name)).String()
}
