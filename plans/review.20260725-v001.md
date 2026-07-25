# OWN REVIEW 20260725 v001
> Добавь глобально правило или скил, который будет анализировать мои замечания и описывать и готовить линтер или скил
> для автопроверки golang кода и переведения его к "нормализованному" стилю.

### это дополнение к задачам [000-review-20260717-backlog.md](000-review-20260717-backlog.md), но тут могут быть дубликаты или дополнения к заданиям.



## T001
`tlsOptions` *(cmd/vpntunnel/main.go:88)*, давай откажемся от этой структуры и будем собирать сразу `tls.Certificate`. 
К примеру, у нас уже есть использования 
```go
	var cert *tls.Certificate
	if tlsOpts.CertDir == "" {
		opLog.Warn("API running WITHOUT TLS — bearer tokens are sent in plaintext; intended for loopback/dev only",
			slog.String("api_listen", cfg.API.Listen),
		)
	} else {
		c, fp, err := apitls.LoadOrGenerate(tlsOpts.CertDir, tlsOpts.Hostname, tlsOpts.IPSANs, opLog)
		if err != nil {
			return fmt.Errorf("load tls cert: %w", err)
		}
		cert = c
		opLog.Info("API TLS enabled",
			slog.String("cert_dir", tlsOpts.CertDir),
			slog.String("sha256_fingerprint", fp),
		)
	}
```
`func parseFlags() (configPath string, tlsOpts tlsOptions, err error)` - думаю можно уже возращать сразу готовый структуры, 
то есть метод должен быть примерно такие `func parseFlags() (config.Config, tls.Certificate, error)`.



## T002
`type runOpt func(*runOptions)` *(cmd/vpntunnel/main.go:67)* - от этого кусока я бы тоже отказался.
из того что я виду это добавленно только для тестов. то есть вот эти определения
- `DeviceBuilder: ro.supervisorBuilder,`
- `DeviceBuilder: ro.schedulerBuilder,`
в коде живут, чтобы тестировать какую-то логику в `main_test.go`.
я бы отказался от этого и перенес логику из кода в тест если это надо.



## T003
*internal/egress* - не нравится мне этот пакет. это описание контракта они должны быть там где испьзется.
например в *internal/application/proxy.go* используется интерфейс `type Dialer interface` из egress.go файла.
надо сделать копию контракта `type Dialer interface` в файл `proxy.go` и итого будет выглядеть так 
```go
package application
...
type ProxyServiceOptions struct {
	...
	// Dialer routes outbound TCP connections. Required.
	Dialer dialer
	...
}
...
type dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}
...
```
подобным способом надо реализовать все контракты internal/egress/egress.go файла и в дальнейшем отказаться и далить сам файл.
так же `Close() error` метод я бы заменил на `io.Closer`.



### T004
структура файла должна строгой, надо переформатировать 

**object.go**
```go
package data

import (...)

const (-- PIUBLIC CONTACTS --)
var (-- PIUBLIC VARIABLES --)

func NewObject() (Object, error)
type Object struct { ... }
func (o *Object) Method1() { ... }
func (o *Object) MethodN() { ... }
func (o *Object) method1() { ... }
func (o *Object) methodN() { ... }

func NewObjectN() (ObjectN, error)
type ObjectN struct { ... }
func (o *Object) Method1() { ... }
func (o *Object) MethodN() { ... }
func (o *Object) method1() { ... }
func (o *Object) methodN() { ... }

type PublicContract1 interface { ... }
type PublicContractN interface { ... }

const (-- PRIVITE CONTACTS --)
var (-- PRIVITE VARIABLES --)

type priviteCotract1 interface { ... }
type priviteCotractN interface { ... }

func helpeer_method1() { ... }
func helpeer_method2() { ... }
```

**object_test.go**
```go
package data

import (
	...
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
	...
)

var _ PublicContract1 = ...
var _ PublicContractN = ...
var _ internalCotract1 = ...
var _ internalCotractN = ...

func TestNewObject(t *testing.T) {
	t.Parallel()
	t.Run("subtest_001", func(t *testing.T) {
		t.Parallel()
		...
	})
	t.Run("subtest_002", func(t *testing.T) {
		t.Parallel()
		...
	})
	...
	t.Run("subtest_NNN", func(t *testing.T) {
		t.Parallel()
		...
	})~~~~
}
func TestObjectMethodN(t *testing.T) {
	t.Parallel()
	t.Run("subtest_001", func(t *testing.T) {
		t.Parallel()
		...
	})
	t.Run("subtest_002", func(t *testing.T) {
		t.Parallel()
		...
	})
	...
	t.Run("subtest_NNN", func(t *testing.T) {
		t.Parallel()
		...
	})
}
func TestObjectmethod1(t *testing.T) {
	t.Parallel()
	t.Run("subtest_001", func(t *testing.T) {
		t.Parallel()
		...
	})
	t.Run("subtest_002", func(t *testing.T) {
		t.Parallel()
		...
	})
	...
	t.Run("subtest_NNN", func(t *testing.T) {
		t.Parallel()
		...
	})
}
func TestObjectmethodN(t *testing.T) {
	t.Parallel()
	t.Run("subtest_001", func(t *testing.T) {
		t.Parallel()
		...
	})
	t.Run("subtest_002", func(t *testing.T) {
		t.Parallel()
		...
	})
	...
	t.Run("subtest_NNN", func(t *testing.T) {
		t.Parallel()
		...
	})
}

func Testhelpeer_method1(t *testing.T){
	t.Parallel()
	t.Run("subtest_001", func(t *testing.T) {
		t.Parallel()
		...
	})
	t.Run("subtest_002", func(t *testing.T) {
		t.Parallel()
		...
	})
	...
	t.Run("subtest_NNN", func(t *testing.T) {
		t.Parallel()
		...
	})
}
func Testhelpeer_methodN(t *testing.T){
	t.Parallel()
	t.Run("subtest_001", func(t *testing.T) {
		t.Parallel()
		...
	})
	t.Run("subtest_002", func(t *testing.T) {
		t.Parallel()
		...
	})
	...
	t.Run("subtest_NNN", func(t *testing.T) {
		t.Parallel()
		...
	})
}

type test_cotract1 interface { ... }
type test_cotractN interface { ... }

const (-- PRIVITE CONTACTS --)
var (-- PRIVITE VARIABLES --)

func test_helpeer_method1() { ... }
func test_helpeer_methodN() { ... }
```



### T005
*./internal/constants/constants.go* - надо вынести в *./internal/constants.go* так будет более корректно.
внтурение константы.



### T006
*internal/infrastructure/config/config.go:23* и *internal/infrastructure/ipdeny/ipdeny.go:35*
я бы вынес */internal* может отдельными файлами, так как это константы прилоежния а не реализации и хранить их в реализации плохая идея.



### T007
*internal/infrastructure/notify/dsn.go* - ради чего этот файл был созда? я бы extractIdentity реализию перенес бы в NewTelegram, 
меньше файлов чище реализация!



### T008
*internal/infrastructure/notify/notify.go:27* я думаю это часть домена а не инфраструктуры, 
то есть я думаю это наше внутреннее представление сообщение\события от телеграмм бота.   