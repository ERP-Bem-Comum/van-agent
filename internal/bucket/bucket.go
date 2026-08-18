// Package bucket é a fronteira com o object storage. O agente é o ÚNICO componente que atravessa a
// fronteira entre o bucket e a máquina do banco: a aplicação nunca toca a instância (ADR-0060), e a
// instância nunca fala com a aplicação.
//
// A interface é definida aqui, e não no chamador, porque quem decide o que o agente precisa do
// armazenamento é o agente. A implementação sobre o SDK da AWS entra em fatia própria, com a
// dependência justificada; esta fatia se sustenta na interface e num duplo em memória, que é o que
// torna os critérios de idempotência e de morte no meio exercitáveis sem nuvem e sem dinheiro real.
//
// ⚠️ Nenhum default deste pacote aponta para bucket real. O NOME do bucket não pertence ao código —
// entra por variável de ambiente na instância (ADR-0061 §5), e este repositório é público.
package bucket

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ErrNotFound distingue "não existe" de "não deu para ler". A distinção decide comportamento: um
// objeto ausente no meio do ciclo é concorrência normal (outro processo já o moveu), enquanto falha
// de leitura é problema de infraestrutura que precisa parar o ciclo em vez de ser absorvido.
var ErrNotFound = errors.New("objeto não encontrado")

// Store é o que o agente precisa do armazenamento — nada além.
//
// Não há `Delete` de propósito: o agente NUNCA apaga (ADR-0061 §1). O objeto muda de prefixo, e o
// versionamento do bucket preserva o histórico. Uma interface sem o método é mais forte que uma
// regra escrita, porque o código que apagaria não compila.
type Store interface {
	List(ctx context.Context, prefix string) ([]string, error)
	Get(ctx context.Context, key string) ([]byte, error)
	Put(ctx context.Context, key string, content []byte) error
	// Move reposiciona o objeto. É a transição de estado da remessa: no contrato do ADR-0061 o
	// estado É a localização, então mover não é housekeeping — é a escrita que muda o mundo.
	Move(ctx context.Context, from, to string) error
}

// Prefixes são os cinco do ADR-0061 §1. `sandbox/` não aparece aqui porque só existe no bucket de
// homologação, e escrever no lugar errado deve exigir trocar o BUCKET, não o prefixo.
type Prefixes struct {
	Outbound  string
	Processed string
	Failed    string
	Returns   string
	Status    string
}

// DefaultPrefixes espelha os valores que o core-api usa em `van-s3-config.ts`. São nomes de
// prefixo, não segredos: o que identifica o ambiente é o nome do bucket, que não mora aqui.
func DefaultPrefixes() Prefixes {
	return Prefixes{
		Outbound:  "saida/",
		Processed: "processados/",
		Failed:    "falhas/",
		Returns:   "retorno/",
		Status:    "status/",
	}
}

// Validate cobra que todo prefixo termine em barra.
//
// Sem a barra, `saida` + `ARQUIVO.REM` vira `saidaARQUIVO.REM` — um objeto na raiz do bucket, que
// nenhuma listagem por prefixo encontra. A remessa sumiria sem erro em lugar nenhum.
func (p Prefixes) Validate() error {
	for name, value := range map[string]string{
		"saída":       p.Outbound,
		"processados": p.Processed,
		"falhas":      p.Failed,
		"retorno":     p.Returns,
		"status":      p.Status,
	} {
		if value == "" {
			return fmt.Errorf("prefixo de %s não configurado", name)
		}
		if !strings.HasSuffix(value, "/") {
			return fmt.Errorf("prefixo de %s (%q) não termina em barra", name, value)
		}
	}
	return nil
}

// NameOf extrai o nome do arquivo de uma chave, removendo o prefixo.
func NameOf(key string) string {
	if i := strings.LastIndex(key, "/"); i >= 0 {
		return key[i+1:]
	}
	return key
}

// Memory é um duplo em memória, para teste.
//
// Vive em código de produção (e não sob `_test.go`) porque também serve ao modo de ensaio do
// agente: rodar o ciclo inteiro contra um armazenamento falso é como se verifica uma instalação
// nova sem depositar nada na fila real do banco.
type Memory struct {
	mu      sync.Mutex
	objects map[string][]byte
	// PutErr, quando definido, faz toda escrita falhar. É o que permite exercitar o caminho em que
	// o desfecho não consegue ser publicado.
	PutErr error
}

// NewMemory cria o duplo vazio.
func NewMemory() *Memory {
	return &Memory{objects: map[string][]byte{}}
}

// Seed insere um objeto direto, sem passar pelas checagens — é o que o agente encontraria ao rodar.
func (m *Memory) Seed(key string, content []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = append([]byte(nil), content...)
}

// Keys devolve todas as chaves, ordenadas, para asserção em teste.
func (m *Memory) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.objects))
	for k := range m.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// List devolve as chaves sob um prefixo, em ordem estável.
//
// A ordenação não é cosmética: sem ela o ciclo processaria os arquivos em ordem de mapa, que Go
// randomiza de propósito, e um teste que depende de qual arquivo foi tratado primeiro passaria e
// falharia alternadamente sem que ninguém mudasse nada.
func (m *Memory) List(_ context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []string{}
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Get lê um objeto.
func (m *Memory) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	content, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return append([]byte(nil), content...), nil
}

// Put escreve um objeto.
func (m *Memory) Put(_ context.Context, key string, content []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.PutErr != nil {
		return m.PutErr
	}
	m.objects[key] = append([]byte(nil), content...)
	return nil
}

// Move reposiciona um objeto.
func (m *Memory) Move(_ context.Context, from, to string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	content, ok := m.objects[from]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, from)
	}
	m.objects[to] = content
	delete(m.objects, from)
	return nil
}
