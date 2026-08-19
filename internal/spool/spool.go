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
	"runtime"
	"sort"
	"strings"
	"time"
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
	// ReadTransferLog devolve o conteúdo do log posicional de transferências, e se ele chegou a ser
	// lido.
	//
	// Os dois retornos existem porque conteúdo vazio tem DUAS causas que significam coisas opostas:
	// nenhum arquivo casou o padrão (não se sabe nada) ou o log existe e está vazio. Colapsar as
	// duas em `""` faria o agente publicar "sei que não havia linha" sobre um caso em que ele não
	// leu log nenhum.
	ReadTransferLog() (raw string, read bool, err error)
	// ListInbound lista os arquivos que o cliente deixou na pasta de ENTRADA.
	ListInbound() ([]string, error)
	// ReadInbound lê um arquivo recebido, sem interpretá-lo. O agente NUNCA abre CNAB.
	ReadInbound(fileName string) ([]byte, error)
	// Archive tira o arquivo da pasta de ENTRADA depois de ele estar no bucket.
	//
	// Sem isto o mesmo arquivo seria reprocessado a cada passada — o agente roda a cada 5 minutos —
	// e "reapareceu" deixaria de ter significado: não haveria como distinguir o banco reenviando um
	// arquivo de ninguém ter tirado o antigo da pasta.
	//
	// ⚠️ Mover na MÁQUINA não é apagar no BUCKET. A regra do ADR-0061 §1 é sobre o bucket, e é por
	// isso que `bucket.Store` não tem remoção. Aqui o conteúdo já está no bucket antes de o arquivo
	// sair da pasta — a ordem não é negociável.
	Archive(fileName string) error
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
	// InboundDir é a pasta onde o cliente deposita o que RECEBE do banco.
	//
	// Vazia é aceitável: os modos de transmissão e ensaio não precisam dela, e exigi-la faria o boot
	// deles falhar numa instalação que ainda não configurou recepção. Quem cobra é o ciclo de
	// recepção, no boot dele.
	InboundDir string
	// ReceivedDir é para onde o arquivo recebido vai depois de estar no bucket. Mesma regra de
	// obrigatoriedade da pasta de entrada.
	ReceivedDir string
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

// ValidateReception cobra o que SÓ o ciclo de recepção precisa.
//
// Separada de `Validate` de propósito: uma instalação que ainda não configurou recepção precisa
// continuar transmitindo, e um boot que falhasse por causa de uma pasta que o modo em execução não
// usa transformaria trabalho pendente em parada de produção.
func (c Config) ValidateReception() error {
	switch {
	case c.InboundDir == "":
		return errors.New("pasta de entrada do cliente STCP não configurada")
	case c.ReceivedDir == "":
		return errors.New("pasta de arquivados da recepção não configurada")
	case c.InboundDir == c.ReceivedDir:
		// Fossem a mesma, arquivar não tiraria o arquivo da fila de entrada e cada passada o
		// reprocessaria — a cada cinco minutos, para sempre.
		return errors.New("pasta de entrada e pasta de arquivados não podem ser a mesma")
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
//
// O segundo retorno separa "nenhum arquivo casou o padrão" de "o arquivo existe e está vazio". As
// duas produziam `""` e eram indistinguíveis, e a diferença decide o que o agente pode AFIRMAR: sem
// arquivo, ele não leu log algum; com arquivo vazio, ele leu e não havia linha. Só a segunda
// sustenta uma conclusão sobre o que o cliente registrou.
func (d *Dir) ReadTransferLog() (string, bool, error) {
	matches, err := casarLogs(d.cfg.LogDir, d.cfg.TransferLogGlob, caixaImportaNoSistema)
	if err != nil {
		return "", false, fmt.Errorf("procurar log de transferências: %w", err)
	}
	if len(matches) == 0 {
		return "", false, nil
	}
	// Ordem lexicográfica decrescente: o nome do log documentado pelo fabricante começa por data
	// no formato `YYYYMMDD` (§7, p.15), que ordena cronologicamente como texto.
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))

	raw, err := os.ReadFile(matches[0])
	if err != nil {
		// Casou o padrão mas não foi lido: o segundo retorno é `false` porque ele afirma que o
		// conteúdo do log é conhecido, e aqui não é. Saber que o arquivo existe não autoriza
		// nenhuma conclusão sobre o que ele registra.
		return "", false, fmt.Errorf("ler log de transferências %q: %w", matches[0], err)
	}
	return string(raw), true, nil
}

// ListInbound lista os arquivos que o cliente deixou na pasta de ENTRADA.
//
// Diretórios são ignorados, e a ordem é estável pelo mesmo motivo do duplo do bucket: sem ela, dois
// arquivos recebidos no mesmo ciclo seriam tratados em ordem de sistema de arquivos, e uma
// investigação sobre qual foi processado primeiro não se reproduziria.
func (d *Dir) ListInbound() ([]string, error) {
	if d.cfg.InboundDir == "" {
		return nil, errors.New("pasta de entrada do cliente STCP não configurada")
	}
	entries, err := os.ReadDir(d.cfg.InboundDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Pasta ausente NÃO é ciclo vazio: uma instalação com a pasta errada configurada
			// reportaria "nada a receber" para sempre, e ninguém veria os arquivos se acumulando.
			return nil, fmt.Errorf("pasta de entrada %q não existe: %w", d.cfg.InboundDir, err)
		}
		return nil, fmt.Errorf("ler pasta de entrada: %w", err)
	}

	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// ReadInbound lê um arquivo recebido. O conteúdo sai daqui CRU — o agente nunca abre CNAB.
func (d *Dir) ReadInbound(fileName string) ([]byte, error) {
	path, err := safeJoin(d.cfg.InboundDir, fileName)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ler arquivo recebido %q: %w", fileName, err)
	}
	return raw, nil
}

// Archive tira o arquivo da pasta de ENTRADA.
//
// Só é chamado depois de o conteúdo estar no bucket. Se falhar, o arquivo fica onde está e o ciclo
// seguinte o encontra de novo — ruído visível, que é o desfecho certo comparado a perder a evidência
// de um pagamento.
func (d *Dir) Archive(fileName string) error {
	origem, err := safeJoin(d.cfg.InboundDir, fileName)
	if err != nil {
		return err
	}
	destino, err := safeJoin(d.cfg.ReceivedDir, fileName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d.cfg.ReceivedDir, 0o750); err != nil {
		return fmt.Errorf("preparar pasta de arquivados: %w", err)
	}
	// Um homônimo já arquivado NÃO é sobrescrito: ele pode ter conteúdo diferente, e sobrescrever
	// destruiria a única cópia local de um retorno. O carimbo desempata.
	if _, err := os.Stat(destino); err == nil {
		destino = destino + ".recebido-" + time.Now().UTC().Format("20060102T150405Z")
	}
	if err := os.Rename(origem, destino); err != nil {
		return fmt.Errorf("arquivar %q: %w", fileName, err)
	}
	return nil
}

// caixaImportaNoSistema diz se o sistema de arquivos distingue maiúsculas de minúsculas.
//
// É `var` e não constante para que o teste exercite OS DOIS regimes na mesma máquina. Sem isso, o
// caminho do Windows ficaria sem cobertura justamente aqui — que é a plataforma de destino, onde
// ninguém roda a suíte, e onde o defeito existe.
var caixaImportaNoSistema = runtime.GOOS != "windows"

// casarLogs acha os arquivos de `dir` que casam o padrão, respeitando o regime de caixa do sistema.
//
// Por que isto não é `filepath.Glob`: `filepath.Match` do Go é case-sensitive em TODAS as
// plataformas — não há case folding nem no Windows. Mas o filesystem do Windows é
// case-INsensitive, e para quem configura a instalação (e para o Explorer, e para o `dir`, e para o
// próprio cliente) `*.LOG` e `*.log` designam o mesmo conjunto de arquivos.
//
// A divergência é silenciosa e cara: o padrão não casa nada, `ReadTransferLog` devolve "não li log
// nenhum", e o agente publica `logDoCicloLido: false` em todo retorno — comportamento correto, mas
// que significa que a correlação nunca funciona sem que nada emita erro. E o operador não tem por
// que desconfiar: no Linux ele já espera que a caixa importe; no Windows, não.
//
// Onde o sistema distingue caixa, o comportamento NÃO muda. Lá dois nomes que diferem só na caixa
// são dois arquivos, e casar ambos escolheria um deles por acidente de ordenação — o oposto de
// errar para menos.
func casarLogs(dir, padrao string, caixaImporta bool) ([]string, error) {
	// Valida o padrão uma vez, contra um nome qualquer: `filepath.Match` só reporta padrão inválido
	// quando chega a compará-lo, e um padrão malformado precisa virar erro em vez de "não casou
	// nada" — que o chamador leria como ausência de log.
	if _, err := filepath.Match(padrao, "x"); err != nil {
		return nil, fmt.Errorf("padrão de log inválido %q: %w", padrao, err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Pasta de log ausente é "nenhum log", não erro: o log é diagnóstico, e o ciclo não pode
			// parar por causa dele. Quem cobra a existência das pastas é o boot.
			return nil, nil
		}
		return nil, err
	}

	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		nome := e.Name()
		alvo, agulha := nome, padrao
		if !caixaImporta {
			alvo, agulha = strings.ToLower(nome), strings.ToLower(padrao)
		}
		if ok, _ := filepath.Match(agulha, alvo); ok {
			out = append(out, filepath.Join(dir, nome))
		}
	}
	return out, nil
}
