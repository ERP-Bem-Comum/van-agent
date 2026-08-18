// Package spool cobre as pastas que o cliente OFTP usa na máquina: SAÍDA, BACKUP e LOG.
//
// É daqui que sai a EVIDÊNCIA FÍSICA, e ela é o veredito (ADR-0061 §2). A razão de não confiar no
// código de saída do processo está no próprio manual: ele documenta o resultado no LOG (§12, p.30),
// nunca o código de saída do executável. Quem depender do código de saída está supondo.
//
// A regra que governa este pacote é o §5 (p.13): TUDO que estiver na pasta de SAÍDA é enviado, e o
// que sai com sucesso é removido de lá e movido para BACKUP. Duas consequências que não se
// negociam:
//
//   - escrever na pasta de SAÍDA "para testar" É transmitir;
//   - o par (sumiu da SAÍDA, apareceu em BACKUP) é a prova de que o arquivo saiu — mais forte que
//     qualquer código, porque é o próprio cliente que a produz ao executar o que promete.
package spool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Spool é o que o ciclo precisa das pastas da instalação.
type Spool interface {
	// Place deposita o arquivo na pasta de SAÍDA. ⚠️ Com o cliente agendado, isto ENFILEIRA para
	// transmissão — não existe "salvar rascunho" ali.
	Place(fileName string, content []byte) error
	// InOutbound reporta se o arquivo continua na pasta de SAÍDA.
	InOutbound(fileName string) (bool, error)
	// InBackup reporta se o arquivo apareceu em BACKUP.
	InBackup(fileName string) (bool, error)
	// ReadTransferLog devolve o conteúdo do log posicional de transferências.
	ReadTransferLog() (string, error)
}

// Config descreve as pastas da instalação. Todos os caminhos entram por ambiente: eles identificam
// perfil e convênio, e este repositório é público.
type Config struct {
	// OutboundDir é a pasta de SAÍDA.
	OutboundDir string
	// BackupDir é a pasta de BACKUP.
	BackupDir string
	// LogDir é a pasta de LOG.
	LogDir string
	// TransferLogGlob casa o arquivo do log POSICIONAL de transferências dentro de LogDir.
	//
	// ⚠️ PENDÊNCIA CONHECIDA: o manual descreve o LAYOUT do log posicional (§12, p.30) e diz que
	// ele fica no diretório de LOG, mas NÃO informa o nome do arquivo. O nome documentado no §7
	// (p.15) é o do log LEGÍVEL, que é outro arquivo, com outro formato. Enquanto o nome real não
	// for observado na instalação, ele é configuração — e a leitura tem uma trava de formato para
	// não decodificar o arquivo errado em silêncio.
	TransferLogGlob string
}

// Validate recusa configuração incompleta no boot.
func (c Config) Validate() error {
	switch {
	case c.OutboundDir == "":
		return errors.New("pasta de saída do cliente STCP não configurada")
	case c.BackupDir == "":
		return errors.New("pasta de backup do cliente STCP não configurada")
	case c.LogDir == "":
		return errors.New("pasta de log do cliente STCP não configurada")
	case c.TransferLogGlob == "":
		return errors.New("padrão do log de transferências não configurado")
	}
	return nil
}

// Dir é a implementação sobre o sistema de arquivos.
type Dir struct {
	cfg Config
}

// NewDir prepara o acesso às pastas.
func NewDir(cfg Config) (*Dir, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Dir{cfg: cfg}, nil
}

// safeJoin impede que um nome vindo do bucket escape da pasta.
//
// O nome já é validado antes de chegar aqui, e mesmo assim a checagem se repete: esta é a última
// fronteira antes de uma escrita em disco na máquina que transmite pagamento, e defesa em
// profundidade custa três linhas.
func safeJoin(dir, fileName string) (string, error) {
	if fileName == "" || strings.ContainsAny(fileName, `/\`) || strings.Contains(fileName, "..") {
		return "", fmt.Errorf("nome de arquivo inseguro: %q", fileName)
	}
	return filepath.Join(dir, fileName), nil
}

// Place deposita o arquivo na pasta de SAÍDA.
//
// Escreve num temporário FORA da pasta de saída e renomeia para dentro. A razão é o §5: o cliente
// envia tudo que estiver lá. Escrever direto criaria uma janela em que um arquivo pela metade está
// na fila, e uma execução do cliente nesse instante transmitiria um arquivo truncado — que o banco
// aceita ou recusa sem que ninguém saiba por quê.
func (d *Dir) Place(fileName string, content []byte) error {
	final, err := safeJoin(d.cfg.OutboundDir, fileName)
	if err != nil {
		return err
	}

	tmpDir := filepath.Dir(d.cfg.OutboundDir)
	tmp, err := os.CreateTemp(tmpDir, ".van-agent-*.tmp")
	if err != nil {
		return fmt.Errorf("criar temporário para %q: %w", fileName, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return fmt.Errorf("escrever %q: %w", fileName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sincronizar %q: %w", fileName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("fechar temporário de %q: %w", fileName, err)
	}
	if err := os.Rename(tmpPath, final); err != nil {
		return fmt.Errorf("mover %q para a pasta de saída: %w", fileName, err)
	}
	return nil
}

func exists(dir, fileName string) (bool, error) {
	path, err := safeJoin(dir, fileName)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("consultar %q: %w", path, err)
}

// InOutbound reporta se o arquivo continua na pasta de SAÍDA.
func (d *Dir) InOutbound(fileName string) (bool, error) { return exists(d.cfg.OutboundDir, fileName) }

// InBackup reporta se o arquivo apareceu em BACKUP.
func (d *Dir) InBackup(fileName string) (bool, error) { return exists(d.cfg.BackupDir, fileName) }

// ReadTransferLog lê o log posicional mais recente que casa com o padrão configurado.
//
// Devolve string vazia, sem erro, quando não há log: o log é DIAGNÓSTICO, e a ausência dele não
// pode impedir a publicação de um desfecho que a evidência física já determinou. Falhar aqui
// deixaria a remessa em estado desconhecido por causa de um arquivo de apoio.
func (d *Dir) ReadTransferLog() (string, error) {
	matches, err := filepath.Glob(filepath.Join(d.cfg.LogDir, d.cfg.TransferLogGlob))
	if err != nil {
		return "", fmt.Errorf("procurar log de transferências: %w", err)
	}
	if len(matches) == 0 {
		return "", nil
	}
	// Ordem lexicográfica decrescente: o nome do log documentado pelo fabricante começa por data
	// no formato `YYYYMMDD` (§7, p.15), que ordena cronologicamente como texto.
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))

	raw, err := os.ReadFile(matches[0])
	if err != nil {
		return "", fmt.Errorf("ler log de transferências %q: %w", matches[0], err)
	}
	return string(raw), nil
}
