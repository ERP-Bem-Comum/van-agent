package ledger

// Envelopes cuja publicação ainda não se confirmou.
//
// A publicação do envelope é a única escrita dos dois ciclos que podia falhar sem que nada a
// repetisse: o erro ia para `Outcome.Err`, e o passo seguinte rodava assim mesmo — a recepção
// arquivava o arquivo (tirando-o da pasta que o ciclo seguinte olha), a transmissão movia o objeto
// (tirando-o da fila). Nos dois casos o registro já dizia `done`, então nada voltava a passar por
// ali e o objeto ficava no bucket sem desfecho publicado, para sempre.
//
// O que se guarda aqui é o CORPO já serializado, e não os dados para reconstruí-lo. Reconstruir
// exigiria o log daquele ciclo, o relógio daquele momento e o estado daquele registro — nenhum dos
// três existe mais depois. Guardar os bytes torna a republicação idêntica por construção, que é o
// que o CA2 pede: o desfecho não mudou, só a publicação falhou.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// PendingEnvelope é uma publicação que começou e não se confirmou.
type PendingEnvelope struct {
	// Chave é o destino no bucket, exatamente como seria na primeira tentativa.
	Chave string `json:"chave"`
	// Corpo é o envelope já serializado. Vai como string porque é JSON UTF-8 e assim o registro
	// continua legível por quem abrir o arquivo na máquina — que é a única forma de investigar uma
	// instalação sem acesso ao bucket.
	Corpo string `json:"corpo"`
	// Arquivo é o nome do arquivo de origem, só para diagnóstico: quem lê a pasta de pendências
	// precisa saber a que remessa ou retorno aquilo se refere sem decodificar o corpo.
	Arquivo      string `json:"arquivo"`
	RegistradoEm string `json:"registradoEm"`
}

// PendingEnvelopes registra publicações não confirmadas.
//
// A ordem de uso é a mesma disciplina da intenção de transmissão, e pela mesma razão: grava-se
// ANTES de tentar. Gravar depois da falha abriria a janela que o mecanismo existe para fechar —
// uma queda entre o `Put` que falhou e o registro deixaria a pendência sem rastro nenhum.
type PendingEnvelopes interface {
	// Save registra a pendência antes da tentativa de publicação.
	Save(key, fileName, body string, at time.Time) error
	// Clear remove a pendência depois de a publicação ter sido confirmada.
	Clear(key string) error
	// List devolve o que continua pendente, em ordem estável.
	List() ([]PendingEnvelope, error)
}

// FilePendingEnvelopes é a implementação em disco.
//
// Diretório PRÓPRIO, como os outros dois registros: aqui se indexa a CHAVE do envelope, enquanto o
// ledger indexa nome de remessa e o índice de recepção indexa hash de conteúdo. Compartilhar
// diretório deixaria aberta uma colisão que custa uma pasta para tornar impossível.
//
// É também o que faz o CA4 valer sem esforço: a reconciliação lista ESTE diretório, que normalmente
// está vazio, em vez de varrer um índice que cresce para sempre.
type FilePendingEnvelopes struct {
	dir string
}

// NewFilePendingEnvelopes prepara o diretório.
func NewFilePendingEnvelopes(dir string) (*FilePendingEnvelopes, error) {
	if dir == "" {
		return nil, errors.New("diretório de envelopes pendentes não configurado")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("criar diretório de envelopes pendentes %q: %w", dir, err)
	}
	return &FilePendingEnvelopes{dir: dir}, nil
}

// path traduz a chave do bucket em caminho local.
//
// O hash existe porque a chave carrega `/` e viraria subdiretório — e um nome vindo do bucket não
// pode decidir onde o agente escreve. O mesmo motivo pelo qual o ledger já derruba o nome de
// remessa em sha256.
func (p *FilePendingEnvelopes) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(p.dir, hex.EncodeToString(sum[:])+".json")
}

// Save registra a pendência ANTES da tentativa de publicação.
func (p *FilePendingEnvelopes) Save(key, fileName, body string, at time.Time) error {
	entry := PendingEnvelope{
		Chave:        key,
		Corpo:        body,
		Arquivo:      fileName,
		RegistradoEm: at.UTC().Format(time.RFC3339),
	}
	raw, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("serializar envelope pendente de %q: %w", key, err)
	}
	return writeDurable(p.path(key), raw, "envelope pendente de "+key)
}

// Clear remove a pendência depois da publicação confirmada.
//
// Ausência NÃO é erro: a limpeza roda depois de toda publicação bem-sucedida, e o caso comum é não
// haver nada para limpar. Tratar isso como falha faria o ciclo acumular erro no caminho feliz.
func (p *FilePendingEnvelopes) Clear(key string) error {
	if err := os.Remove(p.path(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("limpar envelope pendente de %q: %w", key, err)
	}
	return nil
}

// List devolve as pendências em ordem estável.
//
// Registro ilegível NÃO é pulado em silêncio: ele vira erro, porque um arquivo corrompido aqui é
// exatamente um desfecho que ninguém publicou. Ignorá-lo reproduziria o órfão que este mecanismo
// existe para eliminar, agora com um registro em disco dizendo que estava tudo bem.
func (p *FilePendingEnvelopes) List() ([]PendingEnvelope, error) {
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("listar envelopes pendentes: %w", err)
	}

	out := make([]PendingEnvelope, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(p.dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("ler envelope pendente %q: %w", e.Name(), err)
		}
		var entry PendingEnvelope
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, fmt.Errorf("envelope pendente %q ilegível: %w", e.Name(), err)
		}
		out = append(out, entry)
	}

	// Ordem estável pela chave, como no resto do agente: sem ela, uma investigação sobre qual
	// pendência foi republicada primeiro não se reproduziria.
	sort.Slice(out, func(i, j int) bool { return out[i].Chave < out[j].Chave })
	return out, nil
}

// writeDurable grava num temporário, sincroniza e renomeia.
//
// É a mesma sequência do registro de recepção, e pela mesma razão: `os.Rename` no mesmo sistema de
// arquivos é atômico, e o `Sync` é o que faz o registro sobreviver a uma queda de energia — sem
// ele o arquivo pode existir só no cache, e a máquina voltaria sem saber o que ficou por publicar.
func writeDurable(final string, raw []byte, what string) error {
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return fmt.Errorf("criar temporário do %s: %w", what, err)
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return fmt.Errorf("escrever %s: %w", what, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sincronizar %s: %w", what, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("fechar temporário do %s: %w", what, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("efetivar %s: %w", what, err)
	}
	return nil
}
