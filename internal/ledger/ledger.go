// Package ledger guarda a INTENÇÃO de transmitir, e é a peça de maior risco do agente.
//
// A regra que ele existe para garantir: em pagamento, erra-se para MENOS. Um arquivo que talvez
// tenha sido transmitido nunca é retransmitido sozinho — vai para revisão humana. O custo de um
// pagamento atrasado é uma conversa; o de um pagamento em duplicidade é dinheiro do cliente saindo
// duas vezes, e o estorno depende da boa vontade de quem recebeu.
//
// O mecanismo é o registro em disco, gravado ANTES de acionar o cliente e completado DEPOIS de
// conhecer o desfecho. Os três estados possíveis de uma leitura são exatamente as três perguntas
// que o ciclo precisa responder:
//
//	ausente  → nunca tentamos      → pode transmitir
//	intent   → tentamos e não sabemos o desfecho → REVISÃO HUMANA, nunca retransmitir  (CA4)
//	done     → já tentamos e sabemos            → duplicado, não acionar o cliente     (CA3)
//
// Por que disco local, e não o próprio bucket: o registro precisa sobreviver à morte do processo no
// intervalo entre gravar e transmitir, e precisa ser durável ANTES da transmissão. Uma escrita
// remota tem latência e modo de falha próprios justamente na janela que este pacote protege.
package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State é o que se sabe sobre uma tentativa.
type State string

const (
	// StateIntent — a intenção foi gravada e o desfecho é desconhecido. Se o ciclo encontra este
	// estado, houve morte no meio: o processo caiu entre gravar a intenção e registrar o resultado.
	StateIntent State = "intent"
	// StateDone — o desfecho é conhecido e está registrado.
	StateDone State = "done"
)

// Entry é o registro de um nome de arquivo. As tags são PT-BR porque este arquivo é lido por
// humanos em plantão, na máquina, quando algo deu errado — e a pessoa que abre não é quem escreveu.
type Entry struct {
	Arquivo      string `json:"arquivo"`
	Estado       State  `json:"estado"`
	Situacao     string `json:"situacao,omitempty"`
	RegistradoEm string `json:"registradoEm"`
	ConcluidoEm  string `json:"concluidoEm,omitempty"`
}

// Ledger é o contrato do registro. Interface para que o ciclo seja testável com um duplo em
// memória — e para que o teste consiga simular a morte no meio, que é impossível de provocar de
// forma determinística com o disco real.
type Ledger interface {
	// Lookup devolve a entrada e se ela existe.
	Lookup(fileName string) (Entry, bool, error)
	// RecordIntent grava a intenção de forma DURÁVEL, e falha se já existir registro para o nome.
	RecordIntent(fileName string, at time.Time) error
	// RecordDone completa o registro com o desfecho.
	RecordDone(fileName string, situation string, at time.Time) error
}

// ErrAlreadyRecorded é devolvido quando se tenta gravar intenção sobre um nome que já tem registro.
//
// É proteção de corrida, não caminho normal: o ciclo consulta antes. Se este erro aparecer, duas
// execuções do agente estão rodando ao mesmo tempo sobre a mesma máquina — condição que o
// agendamento não deveria produzir e que precisa ser vista, não absorvida.
var ErrAlreadyRecorded = errors.New("já existe registro para este nome de arquivo")

// FileLedger é a implementação em disco.
type FileLedger struct {
	dir string
}

// NewFileLedger prepara o diretório do registro.
func NewFileLedger(dir string) (*FileLedger, error) {
	if dir == "" {
		return nil, errors.New("diretório do registro não configurado")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("criar diretório do registro %q: %w", dir, err)
	}
	return &FileLedger{dir: dir}, nil
}

// path traduz nome de arquivo em caminho de registro.
//
// O nome vira hash em vez de virar nome de arquivo direto por dois motivos independentes: o nome
// vem do bucket e não é confiável como componente de caminho, e o Windows recusa caracteres que um
// nome de remessa poderia conter. O nome original fica DENTRO do JSON, então nada se perde para
// quem for ler.
func (l *FileLedger) path(fileName string) string {
	sum := sha256.Sum256([]byte(fileName))
	return filepath.Join(l.dir, hex.EncodeToString(sum[:])+".json")
}

// Lookup lê o registro de um nome.
func (l *FileLedger) Lookup(fileName string) (Entry, bool, error) {
	raw, err := os.ReadFile(l.path(fileName))
	if errors.Is(err, os.ErrNotExist) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("ler registro de %q: %w", fileName, err)
	}

	var entry Entry
	if err := json.Unmarshal(raw, &entry); err != nil {
		// Registro ilegível NÃO é tratado como ausente. Ausente autorizaria transmitir, e um
		// registro corrompido é justamente o caso em que não se sabe se o arquivo já saiu.
		return Entry{}, false, fmt.Errorf("registro de %q ilegível: %w", fileName, err)
	}
	return entry, true, nil
}

// RecordIntent grava a intenção antes da transmissão.
//
// Usa criação exclusiva (`O_EXCL`) em vez de escrita-e-renomeia: aqui a semântica desejada é
// exatamente "crie se não existir", e o sistema de arquivos já a oferece de forma atômica. A
// alternativa exigiria checar-depois-escrever, com uma janela de corrida no meio.
//
// O `Sync` não é zelo excessivo: sem ele o registro pode existir apenas no cache do sistema quando
// a transmissão começar, e uma queda de energia devolveria a máquina a um estado em que o arquivo
// saiu e o agente não sabe disso — o cenário que este pacote inteiro existe para impedir.
func (l *FileLedger) RecordIntent(fileName string, at time.Time) error {
	entry := Entry{
		Arquivo:      fileName,
		Estado:       StateIntent,
		RegistradoEm: at.UTC().Format(time.RFC3339),
	}
	raw, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("serializar registro de %q: %w", fileName, err)
	}

	f, err := os.OpenFile(l.path(fileName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%w: %s", ErrAlreadyRecorded, fileName)
	}
	if err != nil {
		return fmt.Errorf("criar registro de %q: %w", fileName, err)
	}
	defer f.Close()

	if _, err := f.Write(raw); err != nil {
		return fmt.Errorf("escrever registro de %q: %w", fileName, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sincronizar registro de %q em disco: %w", fileName, err)
	}
	return nil
}

// RecordDone completa o registro com o desfecho.
//
// Escreve num temporário e renomeia: `os.Rename` sobre o mesmo sistema de arquivos é atômico, então
// um leitor concorrente vê o registro antigo ou o novo, nunca meio arquivo. Uma queda entre o
// temporário e a renomeação deixa o estado `intent` de pé — que é o desfecho seguro, porque manda o
// arquivo para revisão em vez de liberá-lo para nova transmissão.
func (l *FileLedger) RecordDone(fileName string, situation string, at time.Time) error {
	existing, found, err := l.Lookup(fileName)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("registro de %q ausente ao concluir", fileName)
	}

	existing.Estado = StateDone
	existing.Situacao = situation
	existing.ConcluidoEm = at.UTC().Format(time.RFC3339)

	raw, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("serializar conclusão de %q: %w", fileName, err)
	}

	final := l.path(fileName)
	tmp := final + ".tmp"

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return fmt.Errorf("criar temporário da conclusão de %q: %w", fileName, err)
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return fmt.Errorf("escrever conclusão de %q: %w", fileName, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sincronizar conclusão de %q: %w", fileName, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("fechar temporário da conclusão de %q: %w", fileName, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("efetivar conclusão de %q: %w", fileName, err)
	}
	return nil
}
