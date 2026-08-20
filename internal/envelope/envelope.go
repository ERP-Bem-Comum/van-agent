// Package envelope carrega o contrato do prefixo `status/` — a ÚNICA janela pela qual o backend
// sabe o que aconteceu com uma remessa depois de depositá-la no bucket (ADR-0060/0061).
//
// Este pacote é a metade produtora de um contrato cuja metade consumidora vive noutro repositório
// (`core-api`, em `src/modules/financial/adapters/van/status-envelope.ts`). As duas metades são
// cobradas contra o MESMO arquivo golden — `testdata/status-envelope.golden.json` aqui, uma cópia
// literal lá. Divergência entre elas é o modo de falha mais caro deste componente: o agente
// publicaria um desfecho que o backend descarta, e a remessa ficaria em estado desconhecido sem
// ninguém errar visivelmente.
//
// Três decisões do contrato que NÃO são detalhe de implementação, e a razão de cada uma:
//
//  1. `logTransferencia` é sempre um array — nunca `null`. O consumidor recusa o envelope inteiro
//     com `van-status-missing-field` se `Array.isArray(logLines)` falhar, e em Go um slice nil
//     serializa como `null`. Um ciclo sem linha de log é normal; um envelope recusado não é.
//  2. `exitCode` é ponteiro porque `null` é semanticamente correto e diferente de `0`: significa que
//     o cliente STCP NÃO chegou a ser executado. É o que se publica na duplicidade. Trocar por `0`
//     diria "executou e deu certo" — a conclusão oposta.
//  3. A chave do duplicado é distinta de propósito. Se sobrescrevesse o status original, uma remessa
//     JÁ transmitida passaria a constar como não transmitida, e o operador reenviaria um pagamento
//     que o banco já recebeu.
//  4. `detalhe` tem teto declarado — `MaxDetailLength`. Ele não existia, e a ausência derrubava o
//     consumidor: a coluna de lá é dimensionada, e em MySQL estrito exceder é ERRO, não
//     truncamento. O `INSERT` falha, a confirmação falha, e a varredura de lá aborta na chave ruim
//     em vez de pulá-la — toda remessa que ordene depois nunca é confirmada (#20).
package envelope

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Situation é o veredito do agente. O consumidor recusa valor fora desta lista com
// `van-status-unknown-situation`, de propósito: uma situação nova exige decisão do outro lado, e
// silenciá-la esconderia a mudança de contrato.
type Situation string

const (
	// Transmitted — evidência física de que o arquivo saiu: sumiu da pasta de SAÍDA e apareceu em
	// BACKUP. É o único valor que autoriza o backend a dar a remessa por transmitida.
	Transmitted Situation = "transmitido"
	// Failed — o arquivo continua na pasta de SAÍDA depois do ciclo. Nada saiu.
	Failed Situation = "falha"
	// Review — desfecho ambíguo ou execução interrompida. Exige olho humano; o agente NUNCA
	// retransmite sozinho o que caiu aqui.
	Review Situation = "revisao"
	// Reception — resultado de um ciclo de recepção, publicado só quando houve arquivo ou erro.
	Reception Situation = "recepcao"
)

// StatusPrefix é fixo no contrato; o nome do BUCKET é que varia por ambiente e entra por
// configuração — nunca por código (ADR-0061 §5).
const StatusPrefix = "status/"

const (
	duplicateMarker = ".duplicado-"
	receptionPrefix = "recepcao-"
)

// StampLayout é o carimbo das chaves de duplicado e recepção. Compacto e ordenável
// lexicograficamente, para que uma listagem do prefixo saia em ordem cronológica sem parsing.
const StampLayout = "20060102T150405Z"

// Envelope é o corpo publicado em `status/`. As tags JSON estão em PT-BR porque o contrato foi
// acordado assim com a infra em 2026-08-10 — o consumidor lê `arquivo`, `executadoEm`, `situacao`,
// `detalhe`, `exitCode` e `logTransferencia`, e renomear qualquer um quebra a leitura do outro lado.
//
// `codigoStcp` NÃO entra aqui. A infra o declarou diagnóstico auxiliar, fora do contrato, e o
// consumidor não o lê; publicá-lo convidaria alguém a depender de um campo que pode sumir.
type Envelope struct {
	Arquivo          string    `json:"arquivo"`
	ExecutadoEm      string    `json:"executadoEm"`
	Situacao         Situation `json:"situacao"`
	Detalhe          string    `json:"detalhe"`
	ExitCode         *int      `json:"exitCode"`
	LogTransferencia []string  `json:"logTransferencia"`
	// Recepcao só existe em envelope de recepção, e é OMITIDO nos demais.
	//
	// O `omitempty` não é economia de bytes: ele é o que faz os envelopes de remessa continuarem
	// byte a byte idênticos aos que o consumidor já aceita. A adição fica contida no único caso que
	// precisava dela, e o resto do contrato não se mexe.
	Recepcao *ReceptionInfo `json:"recepcao,omitempty"`
}

// ReceptionInfo é a proveniência de um arquivo recebido do banco.
//
// Ela existe porque a caixa é do CONVÊNIO, não da nossa remessa: chegam arquivos de lotes que não
// são nossos. A regra que o core-api vai aplicar é "só processa objeto que tenha envelope de
// recepção correspondente", e o que aparecer sem envelope vai para quarentena visível — nem
// processado, nem descartado em silêncio. Para isso ele precisa saber, sem reabrir o objeto, o que
// foi recebido, com que conteúdo e com que evidência.
type ReceptionInfo struct {
	// Sha256 é o hash do CONTEÚDO, em hexadecimal minúsculo.
	//
	// É por ele que se separa "o banco reenviou o mesmo arquivo" de "chegou arquivo novo com nome
	// homônimo". O NOME não serve: quem o atribui é o banco, e deduplicar por nome produziria as
	// duas falhas opostas — descartar arquivo novo e aceitar reenvio como novidade.
	Sha256 string `json:"sha256"`
	// Chave é onde o objeto foi depositado, para que o consumidor não precise adivinhar.
	Chave string `json:"chave"`
	// Correlacionado diz se o arquivo casou com alguma linha de recepção do log DESTE ciclo.
	//
	// `false` NÃO significa "não é confiável": significa que o agente não conseguiu provar a origem
	// pelo log, e mesmo assim depositou. Erra-se para mais aqui: descartar em silêncio um arquivo do
	// banco é o desfecho que ninguém percebe.
	//
	// ⚠️ Só é interpretável junto com `LogDoCicloLido`. Sozinho, `false` mistura "o log deste ciclo
	// não foi lido" com "foi lido e não havia a linha" — duas coisas que levam a ações opostas.
	Correlacionado bool `json:"correlacionado"`
	// LogDoCicloLido diz se o agente leu o log DESTA execução — ou seja, se `Correlacionado` carrega
	// uma conclusão ou apenas uma ignorância.
	//
	// Ele existe porque o nome do log começa por data (§7, p.15) e o padrão casa o mais recente: no
	// primeiro ciclo do dia, antes de o cliente escrever o log novo, o agente lê com sucesso o log
	// de ONTEM. Sem este campo, esse caso — que acontece todo dia — sairia indistinguível de "o
	// cliente não registrou este arquivo", e um consumidor que represa por não-correlação represaria
	// todo retorno do primeiro ciclo, diariamente.
	//
	// A leitura acordada com o core-api (2026-08-19):
	//
	//	true  + Correlacionado false → origem não registrada pelo cliente: suspeito, revisar
	//	false                        → o agente não sabe; NÃO é sinal sobre o arquivo, e sim sobre a
	//	                               configuração do agente (padrão do log, pasta, permissão)
	//
	// Um booleano que chutasse aqui seria pior que campo nenhum: o consumidor obedeceria a um sinal
	// errado achando que é preciso.
	LogDoCicloLido bool `json:"logDoCicloLido"`
	// Duplicado marca a recepção de um conteúdo que já havia sido recebido antes.
	Duplicado bool `json:"duplicado,omitempty"`
	// DuplicadoDe é a chave da recepção anterior com o mesmo conteúdo — o que permite ao consumidor
	// ligar as duas sem reabrir objeto nenhum.
	DuplicadoDe string `json:"duplicadoDe,omitempty"`
}

// MaxDetailLength é o teto do campo `detalhe`, em CARACTERES.
//
// Caracteres, não bytes, porque é assim que o consumidor conta: a coluna de lá é `varchar(512)` e o
// `varchar` do MySQL dimensiona em caracteres. Medir aqui em bytes seria conservador por ~2% num
// texto PT-BR acentuado — erraria para o lado seguro, mas erraria, e um teto que não é o teto do
// outro lado deixa de ser cláusula de contrato para virar palpite.
//
// O número é acordado com o core-api (#20) e vale nos dois sentidos: aqui o produtor TRUNCA e
// MARCA; lá o consumidor DEFENDE, garantindo que exceder nunca derrube o desfecho. As duas metades
// protegem coisas diferentes — esta protege a informação, aquela protege o registro do pagamento.
const MaxDetailLength = 512

// truncationMark fecha um `detalhe` que não coube.
//
// A marca é inegociável, e não é cosmética: diagnóstico cortado em SILÊNCIO é pior que diagnóstico
// ausente, porque parece completo. Quem lê um detalhe truncado sem aviso conclui que o erro
// terminou ali, e para de procurar exatamente onde a informação começava a faltar.
const truncationMark = " […]"

// truncateDetail garante o teto do contrato.
//
// Roda no construtor, e não em cada chamador, porque é a única posição em que a garantia é
// INESCAPÁVEL: todo envelope passa por aqui. Deixar a responsabilidade no ponto de interpolação
// significaria que um chamador novo, escrito daqui a seis meses, reabre o defeito sem que nada
// acuse — e o que acusaria seria a varredura do outro repositório travando em produção.
//
// O corte é por RUNA, nunca por byte: fatiar bytes partiria um caractere acentuado ao meio e
// produziria JSON inválido para o consumidor. O texto deste agente é PT-BR — isso não é hipótese
// remota, é o caso comum.
//
// A escolha de ONDE cortar mora no pacote `agent` (`resumirErro`): lá se sabe qual parte da
// frase é instrução ao operador e qual é cauda de erro do sistema operacional. Aqui é só o piso.
func truncateDetail(detail string) string {
	if utf8.RuneCountInString(detail) <= MaxDetailLength {
		return detail
	}
	runas := []rune(detail)
	return string(runas[:MaxDetailLength-utf8.RuneCountInString(truncationMark)]) + truncationMark
}

// New monta um envelope com os invariantes já garantidos, e é o único construtor exportado de
// propósito: montar a struct na mão permitiria um `LogTransferencia` nil, que serializa como `null`
// e faz o consumidor recusar o envelope inteiro.
func New(fileName string, at time.Time, situation Situation, detail string, exitCode *int, logLines []string) Envelope {
	lines := logLines
	if lines == nil {
		lines = []string{}
	}
	return Envelope{
		Arquivo: fileName,
		// UTC com sufixo Z: a máquina fica em fuso do Brasil, e um horário sem fuso explícito num
		// componente que decide pagamento é ambiguidade que aparece só no dia da divergência.
		ExecutadoEm:      at.UTC().Format(time.RFC3339),
		Situacao:         situation,
		Detalhe:          truncateDetail(detail),
		ExitCode:         exitCode,
		LogTransferencia: lines,
	}
}

// NewReception monta o envelope de um arquivo recebido.
//
// Construtor próprio pelo mesmo motivo de `New` ser o único exportado: montar a struct na mão
// deixaria `LogTransferencia` nil (que serializa como `null` e faz o consumidor recusar o envelope
// inteiro) ou `Recepcao` nil num envelope que precisa dele.
func NewReception(
	fileName string,
	at time.Time,
	situation Situation,
	detail string,
	exitCode *int,
	logLines []string,
	reception ReceptionInfo,
) Envelope {
	env := New(fileName, at, situation, detail, exitCode, logLines)
	env.Recepcao = &reception
	return env
}

// Marshal serializa em UTF-8 sem BOM, para que o consumidor possa dar `JSON.parse` direto.
//
// `SetEscapeHTML(false)` desliga o escape que o encoder do Go aplica por padrão a `<`, `>` e `&`:
// ele é defesa para JSON embutido em HTML, e aqui só produziria `<` no meio de uma mensagem de
// erro do STCP — ilegível para quem abrir o objeto no console do bucket.
func Marshal(e Envelope) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(e); err != nil {
		return nil, fmt.Errorf("serializar envelope de %q: %w", e.Arquivo, err)
	}
	return buf.Bytes(), nil
}

// Key é a chave do resultado normal: um objeto por remessa.
func Key(fileName string) string {
	return StatusPrefix + fileName + ".json"
}

// DuplicateKey é a chave da tentativa recusada por nome já processado.
//
// O carimbo existe para NÃO sobrescrever o status original — e é por isso que ele nunca colide com
// `Key`: a remessa que já saiu continua legível como transmitida, e a tentativa repetida fica ao
// lado, visível, sem apagar o histórico.
func DuplicateKey(fileName string, at time.Time) string {
	return StatusPrefix + fileName + duplicateMarker + at.UTC().Format(StampLayout) + ".json"
}

// ReceptionKey é a chave do envelope de UM arquivo recebido.
//
// Publicada SOMENTE quando houve arquivo recebido ou erro. O agente roda a cada 5 minutos; publicar
// sempre geraria centenas de objetos vazios por dia. A ausência significa "rodou e não havia nada a
// receber" — quem consumir não pode ler silêncio como falha.
//
// O NOME entra na chave, e não só o carimbo, porque o envelope é por ARQUIVO: ele carrega o hash e
// as linhas que correlacionam aquele arquivo específico. Com a chave só de carimbo, dois arquivos
// recebidos no mesmo segundo colidiriam — e o segundo apagaria a evidência do primeiro.
//
// O carimbo continua vindo primeiro para que a listagem do prefixo saia em ordem cronológica sem
// parsing, e é ele que garante que uma recepção repetida NUNCA sobrescreva a anterior.
func ReceptionKey(fileName string, at time.Time) string {
	return StatusPrefix + receptionPrefix + at.UTC().Format(StampLayout) + "-" + fileName + ".json"
}

// ValidName recusa nome de arquivo que produziria chave inválida ou que escaparia do prefixo.
//
// É guarda de fronteira, não validação de negócio: o nome chega do bucket, e um objeto cujo nome
// contenha `/` ou `..` viraria escrita fora de `status/`. Quem grava o objeto de saída é o backend,
// mas o agente não confia nele por isso — confiar exigiria que os dois evoluíssem juntos para
// sempre.
func ValidName(fileName string) error {
	switch {
	case fileName == "":
		return fmt.Errorf("nome de arquivo vazio")
	case strings.ContainsAny(fileName, `/\`):
		return fmt.Errorf("nome de arquivo %q contém separador de caminho", fileName)
	case strings.Contains(fileName, ".."):
		return fmt.Errorf("nome de arquivo %q contém travessia de diretório", fileName)
	case strings.Contains(fileName, duplicateMarker):
		// Um nome carregando o marcador produziria chave indistinguível de um status de duplicado,
		// e o consumidor classifica pela CHAVE — a remessa passaria a ser lida como tentativa
		// recusada.
		return fmt.Errorf("nome de arquivo %q contém o marcador de duplicado %q", fileName, duplicateMarker)
	case strings.HasPrefix(fileName, receptionPrefix):
		// Mesmo motivo, do outro lado: viraria um status de recepção aos olhos do consumidor.
		return fmt.Errorf("nome de arquivo %q começa com o prefixo de recepção %q", fileName, receptionPrefix)
	}
	return nil
}
