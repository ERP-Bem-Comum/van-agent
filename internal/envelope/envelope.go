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
package envelope

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
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
		Detalhe:          detail,
		ExitCode:         exitCode,
		LogTransferencia: lines,
	}
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

// ReceptionKey é a chave do ciclo de recepção.
//
// Publicada SOMENTE quando houve arquivo recebido ou erro. O agente roda a cada 5 minutos; publicar
// sempre geraria centenas de objetos vazios por dia. A ausência significa "rodou e não havia nada a
// receber" — quem consumir não pode ler silêncio como falha.
func ReceptionKey(at time.Time) string {
	return StatusPrefix + receptionPrefix + at.UTC().Format(StampLayout) + ".json"
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
