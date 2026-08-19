// Package stcp cobre a fronteira com o cliente OFTP do banco: acioná-lo por linha de comando e ler
// o log posicional que ele deixa.
//
// FONTE PRIMÁRIA: manual BRADESCO STCP OFTP Client v5.3 (06/2023), §6 p.14 (linha de comando) e
// §12 p.30 (layout do log de transferências). O material é local-only, com restrição de
// redistribuição — por isso este arquivo cita SEÇÃO E PÁGINA e reproduz apenas o que é fato
// estrutural necessário ao código (larguras e códigos), nunca trechos do texto.
//
// ⚠️ O log NÃO é a fonte da verdade sobre a transmissão. O manual não documenta o código de saída do
// processo, e o ADR-0061 §2 decide o veredito por EVIDÊNCIA FÍSICA — o arquivo sumir da pasta de
// SAÍDA e aparecer em BACKUP. O que este parser produz é diagnóstico: alimenta `detalhe` e
// `logTransferencia` do envelope. Um erro de offset aqui degrada a mensagem; não inverte o veredito.
package stcp

import (
	"strings"
)

// Códigos de operação do campo 2 (§12, p.30).
const (
	OpInboundSessionStart  = "0000"
	OpInboundSessionEnd    = "0001"
	OpOutboundSessionStart = "0002"
	OpOutboundSessionEnd   = "0003"
	OpSendStart            = "0004"
	OpSendEnd              = "0005"
	OpReceiveStart         = "0006"
	OpReceiveEnd           = "0007"
)

// ResultSuccess é o único valor de sucesso do campo 7 (§12, p.30). Todo o resto é erro, e a tabela
// que os explica é a do §11 (pp. 24-29).
const ResultSuccess = "000000"

// LineWidth é a soma das dez larguras do §12. Serve de sanidade: uma linha muito mais curta indica
// que o arquivo lido não é o log posicional — provavelmente é o log LEGÍVEL do §7 (p.15), que mora
// no mesmo diretório e tem formato completamente diferente.
const LineWidth = 482

// field descreve um campo do layout por posição inicial e largura, na ordem do §12. Manter a tabela
// explícita (em vez de offsets espalhados pelo parser) é o que permite conferir o código contra a
// página do manual sem reconstruir a aritmética.
type field struct {
	start int
	width int
}

var (
	fOccurredAt = field{0, 14}    // 1 · N · YYYYMMDDhhmmss
	fOp         = field{14, 4}    // 2 · N · código da operação
	fProfile    = field{18, 30}   // 3 · X · nome do perfil
	fProcess    = field{48, 16}   // 4 · X · nome do processo de comunicação
	fProcessID  = field{64, 8}    // 5 · X · código do processo
	fThreadID   = field{72, 8}    // 6 · X · código da thread
	fResult     = field{80, 6}    // 7 · N · resultado
	fSize       = field{86, 12}   // 8 · N · tamanho do arquivo
	fFileName   = field{98, 256}  // 9 · X · nome do arquivo
	fInfo       = field{354, 128} // 10 · X · informações gerais
)

// Record é uma linha decodificada. `Raw` é preservado porque é ELE que vai para o envelope: o
// consumidor recebe as linhas cruas de propósito, para que uma decodificação errada do nosso lado
// não apague a evidência de que alguém precise depois.
type Record struct {
	OccurredAt string
	Op         string
	Profile    string
	Process    string
	ProcessID  string
	ThreadID   string
	Result     string
	Size       string
	FileName   string
	Info       string
	Raw        string
}

// Succeeded reporta se a linha registra sucesso, conforme o campo 7.
func (r Record) Succeeded() bool { return r.Result == ResultSuccess }

// slice recorta um campo tolerando linha curta.
//
// A tolerância é deliberada: o parser NUNCA falha por linha malformada. Ele roda depois de uma
// transmissão que já aconteceu — abortar aqui trocaria um diagnóstico incompleto por um desfecho
// desconhecido, e desfecho desconhecido em pagamento custa mais que mensagem pobre.
func slice(line string, f field) string {
	if f.start >= len(line) {
		return ""
	}
	end := f.start + f.width
	if end > len(line) {
		end = len(line)
	}
	return strings.TrimSpace(line[f.start:end])
}

// ParseLine decodifica uma linha do log posicional.
func ParseLine(line string) Record {
	return Record{
		OccurredAt: slice(line, fOccurredAt),
		Op:         slice(line, fOp),
		Profile:    slice(line, fProfile),
		Process:    slice(line, fProcess),
		ProcessID:  slice(line, fProcessID),
		ThreadID:   slice(line, fThreadID),
		Result:     slice(line, fResult),
		Size:       slice(line, fSize),
		FileName:   slice(line, fFileName),
		Info:       slice(line, fInfo),
		Raw:        line,
	}
}

// ParseLog decodifica o conteúdo inteiro, descartando linhas vazias.
//
// Aceita CRLF e LF: o arquivo é escrito por um programa Windows, mas pode chegar aqui por uma
// cópia que normalizou o terminador, e recusar por causa de um `\r` deixaria o ciclo sem
// diagnóstico nenhum.
func ParseLog(content string) []Record {
	raw := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	records := make([]Record, 0, len(raw))
	for _, line := range raw {
		if strings.TrimSpace(line) == "" {
			continue
		}
		records = append(records, ParseLine(line))
	}
	return records
}

// FilterByFile devolve as linhas cujo campo 9 casa EXATAMENTE com o nome informado.
//
// A correlação é por nome de arquivo porque é o único identificador que atravessa a fronteira: o
// agente escolhe o nome, o cliente o registra no log, e o banco o usa para identificar tipo e fila.
// Comparação exata, nunca prefixo — dois arquivos do mesmo dia compartilham prefixo, e atribuir a
// linha de um ao outro produziria o pior desfecho possível: dar por transmitida uma remessa que não
// saiu.
func FilterByFile(records []Record, fileName string) []Record {
	out := make([]Record, 0, len(records))
	for _, r := range records {
		if r.FileName == fileName {
			out = append(out, r)
		}
	}
	return out
}

// RawLines extrai o texto cru, que é o que viaja no envelope.
func RawLines(records []Record) []string {
	out := make([]string, 0, len(records))
	for _, r := range records {
		out = append(out, r.Raw)
	}
	return out
}

// SendOutcome resume o que o log diz sobre a transmissão de um arquivo.
//
// É RESUMO, não veredito. Quem decide a situação do envelope é a evidência física, no pacote
// `agent`; isto aqui só explica o que aconteceu, para o campo `detalhe`.
type SendOutcome struct {
	Started   bool
	Finished  bool
	Succeeded bool
	// FailureCode é o campo 7 da linha de fim de transmissão quando ela não traz sucesso. Vale
	// contra a tabela do §11 (pp. 24-29).
	FailureCode string
}

// SendOutcomeFor lê as linhas de UM arquivo e resume a transmissão.
func SendOutcomeFor(records []Record, fileName string) SendOutcome {
	var out SendOutcome
	for _, r := range FilterByFile(records, fileName) {
		switch r.Op {
		case OpSendStart:
			out.Started = true
		case OpSendEnd:
			out.Finished = true
			if r.Succeeded() {
				out.Succeeded = true
				// Um fim de transmissão bem-sucedido apaga código de falha de tentativa anterior:
				// o cliente é configurado com retentativas (§6, p.14, parâmetro `-r`), e a última
				// palavra é a que vale.
				out.FailureCode = ""
			} else {
				out.FailureCode = r.Result
			}
		}
	}
	return out
}

// ReceptionLinesFor devolve as linhas de RECEPÇÃO daquele arquivo.
//
// A correlação por linha de log é o filtro mais forte disponível, e a razão é a origem: quem
// escreve essas linhas é o cliente do banco, não um palpite nosso sobre o nome do arquivo — que é
// atribuído pelo banco, não por nós. A caixa é do CONVÊNIO, e chegam arquivos de lotes que não são
// nossos; sem um critério vindo do próprio cliente, não haveria como dizer o que aquela execução
// recebeu.
//
// Só as operações de recepção entram (§12, p.30). Uma linha de TRANSMISSÃO do mesmo nome
// correlacionaria o arquivo errado — e um retorno tratado como remessa é o tipo de confusão que
// aparece semanas depois.
func ReceptionLinesFor(records []Record, fileName string) []Record {
	out := make([]Record, 0, len(records))
	for _, r := range FilterByFile(records, fileName) {
		if r.Op == OpReceiveStart || r.Op == OpReceiveEnd {
			out = append(out, r)
		}
	}
	return out
}

// ReceivedFileNames lista os nomes que aparecem em linhas de recepção, sem repetir.
//
// Serve ao caso inverso do `ReceptionLinesFor`: o log diz que um arquivo foi recebido e ele NÃO
// está na pasta. É informação de diagnóstico — o agente não inventa arquivo a partir do log —, mas
// precisa aparecer, porque um arquivo que o cliente diz ter recebido e sumiu antes de o agente
// olhar é exatamente o que ninguém percebe.
func ReceivedFileNames(records []Record) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, r := range records {
		if r.Op != OpReceiveStart && r.Op != OpReceiveEnd {
			continue
		}
		if r.FileName == "" || seen[r.FileName] {
			continue
		}
		seen[r.FileName] = true
		out = append(out, r.FileName)
	}
	return out
}
