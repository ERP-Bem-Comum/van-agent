package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ReceptionEntry é o registro de um arquivo recebido do banco.
//
// A chave é o SHA-256 do CONTEÚDO, não o nome. O nome é atribuído pelo banco: o mesmo arquivo pode
// voltar com nome diferente, e nomes iguais podem trazer conteúdo diferente. Só o hash separa
// "reenvio do mesmo arquivo" de "arquivo novo com nome homônimo" — indexar por nome produziria as
// duas falhas opostas, descartar arquivo novo e aceitar reenvio como novidade.
//
// As tags são PT-BR porque este arquivo é lido por gente em plantão, na máquina, quando algo deu
// errado — e quem abre não é quem escreveu.
type ReceptionEntry struct {
	Sha256       string `json:"sha256"`
	Arquivo      string `json:"arquivo"`
	Estado       State  `json:"estado"`
	Chave        string `json:"chave,omitempty"`
	RegistradoEm string `json:"registradoEm"`
	ConcluidoEm  string `json:"concluidoEm,omitempty"`
}

// ReceptionIndex é o registro do que já foi recebido.
//
// Os dois estados significam o mesmo que na transmissão, e pela mesma razão:
//
//	ausente  → nunca vimos este conteúdo        → depositar
//	intent   → começamos e não sabemos o desfecho → REVISÃO explícita, nunca sucesso presumido
//	done     → já depositamos                    → recepção duplicada, não redepositar
type ReceptionIndex interface {
	// Lookup consulta pelo hash do conteúdo.
	Lookup(sum string) (ReceptionEntry, bool, error)
	// RecordIntent grava, de forma durável, que o depósito vai começar.
	RecordIntent(sum, fileName string, at time.Time) error
	// RecordDone completa o registro com a chave em que o objeto foi depositado.
	RecordDone(sum, key string, at time.Time) error
}

// FileReceptionIndex é a implementação em disco.
//
// Ela vive num DIRETÓRIO PRÓPRIO, e não ao lado do registro de remessas. Os dois indexam coisas
// diferentes — um o nome do arquivo de saída, o outro o hash do conteúdo de entrada — e um
// diretório compartilhado deixaria aberta a colisão entre um nome de remessa e um hash: improvável,
// mas o custo de tornar impossível é uma pasta.
type FileReceptionIndex struct {
	dir string
}

// NewFileReceptionIndex prepara o diretório do índice.
func NewFileReceptionIndex(dir string) (*FileReceptionIndex, error) {
	if dir == "" {
		return nil, errors.New("diretório do índice de recepção não configurado")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("criar diretório do índice de recepção %q: %w", dir, err)
	}
	return &FileReceptionIndex{dir: dir}, nil
}

// path traduz o hash em caminho.
//
// O hash já é hexadecimal e seguro como nome de arquivo — não há o que sanitizar, ao contrário do
// nome de remessa, que vem do bucket. A validação continua existindo para que um valor vindo de
// outro lugar não vire escrita fora do diretório.
func (i *FileReceptionIndex) path(sum string) (string, error) {
	if !isHexSum(sum) {
		return "", fmt.Errorf("hash de conteúdo inválido: %q", sum)
	}
	return filepath.Join(i.dir, sum+".json"), nil
}

func isHexSum(sum string) bool {
	if len(sum) != 64 {
		return false
	}
	for _, r := range sum {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// Lookup consulta pelo hash do conteúdo.
func (i *FileReceptionIndex) Lookup(sum string) (ReceptionEntry, bool, error) {
	path, err := i.path(sum)
	if err != nil {
		return ReceptionEntry{}, false, err
	}

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ReceptionEntry{}, false, nil
	}
	if err != nil {
		return ReceptionEntry{}, false, fmt.Errorf("ler registro de recepção %q: %w", sum, err)
	}

	var entry ReceptionEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		// Registro ilegível NÃO é tratado como ausente. Ausente autorizaria depositar de novo, e um
		// registro corrompido é justamente o caso em que não se sabe o que já aconteceu.
		return ReceptionEntry{}, false, fmt.Errorf("registro de recepção de %q ilegível: %w", sum, err)
	}
	return entry, true, nil
}

// RecordIntent grava a intenção antes do depósito.
//
// Diferente da transmissão, aqui a intenção NÃO usa criação exclusiva: o mesmo conteúdo pode
// reaparecer legitimamente, e um erro de "já existe" transformaria o caso normal de duplicidade num
// erro de execução. Quem decide o que fazer com um registro existente é o ciclo, que consultou
// antes.
func (i *FileReceptionIndex) RecordIntent(sum, fileName string, at time.Time) error {
	entry := ReceptionEntry{
		Sha256:       sum,
		Arquivo:      fileName,
		Estado:       StateIntent,
		RegistradoEm: at.UTC().Format(time.RFC3339),
	}
	return i.write(sum, entry)
}

// RecordDone completa o registro com a chave em que o objeto foi depositado.
func (i *FileReceptionIndex) RecordDone(sum, key string, at time.Time) error {
	existing, found, err := i.Lookup(sum)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("registro de recepção de %q ausente ao concluir", sum)
	}

	existing.Estado = StateDone
	existing.Chave = key
	existing.ConcluidoEm = at.UTC().Format(time.RFC3339)
	return i.write(sum, existing)
}

// write grava num temporário e renomeia.
//
// `os.Rename` sobre o mesmo sistema de arquivos é atômico: um leitor concorrente vê o registro
// antigo ou o novo, nunca meio arquivo. Uma queda entre o temporário e a renomeação deixa o estado
// anterior de pé — e o estado anterior é sempre o mais conservador dos dois.
func (i *FileReceptionIndex) write(sum string, entry ReceptionEntry) error {
	final, err := i.path(sum)
	if err != nil {
		return err
	}

	raw, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("serializar registro de recepção de %q: %w", sum, err)
	}

	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return fmt.Errorf("criar temporário do registro de recepção de %q: %w", sum, err)
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return fmt.Errorf("escrever registro de recepção de %q: %w", sum, err)
	}
	// O `Sync` é o que faz o registro sobreviver a uma queda de energia. Sem ele o arquivo pode
	// existir apenas no cache do sistema, e a máquina voltaria sem saber o que já recebeu.
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sincronizar registro de recepção de %q: %w", sum, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("fechar temporário do registro de recepção de %q: %w", sum, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("efetivar registro de recepção de %q: %w", sum, err)
	}
	return nil
}
