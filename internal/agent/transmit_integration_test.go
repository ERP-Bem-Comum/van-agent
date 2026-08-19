package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/ERP-Bem-Comum/van-agent/internal/agent"
	"github.com/ERP-Bem-Comum/van-agent/internal/bucket"
	"github.com/ERP-Bem-Comum/van-agent/internal/config"
	"github.com/ERP-Bem-Comum/van-agent/internal/envelope"
	"github.com/ERP-Bem-Comum/van-agent/internal/ledger"
	"github.com/ERP-Bem-Comum/van-agent/internal/spool"
	"github.com/ERP-Bem-Comum/van-agent/internal/stcp/stcpfake"
)

// O ciclo inteiro contra um object storage REAL — o que a fatia 1 não conseguia fazer.
//
// O que este teste cobre e o duplo em memória não cobre: que a chave montada é a chave gravada, que
// a listagem por prefixo enxerga o que o emissor depositou, que o status publicado é legível do
// outro lado, e que a movimentação para `processados/` de fato tira o objeto da fila. Um erro em
// qualquer um destes pontos passaria despercebido contra `bucket.Memory`, porque o duplo é fiel ao
// CONTRATO — não ao S3.
//
// O cliente STCP continua sendo o duplo, e continua sendo por uma razão que não muda: não há
// ambiente de homologação, e a única conexão existente é a de produção no convênio real.
//
// Pulado (nunca falho) sem endpoint configurado: a suíte roda numa máquina sem nuvem e sem rede.
func TestCA1_CicloCompletoContraObjectStorageReal(t *testing.T) {
	endpoint := os.Getenv("VAN_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("VAN_S3_ENDPOINT não configurado — sem object storage para atravessar")
	}
	keyID, secret := os.Getenv("VAN_S3_ACCESS_KEY_ID"), os.Getenv("VAN_S3_SECRET_ACCESS_KEY")
	if keyID == "" || secret == "" {
		t.Skip("credencial do endpoint não configurada — o teste não inventa chave")
	}

	ctx := context.Background()
	bucketName := fmt.Sprintf("van-agent-ciclo-%d", time.Now().UnixNano())
	t.Setenv("VAN_S3_BUCKET", bucketName)

	storageCfg, err := config.LoadStorage()
	if err != nil {
		t.Fatalf("ler configuração do armazenamento: %v", err)
	}

	raw := clienteDeApoio(t, ctx, storageCfg)
	if _, err := raw.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucketName)}); err != nil {
		t.Fatalf("criar bucket de teste: %v", err)
	}
	t.Cleanup(func() { limpaBucket(t, raw, bucketName) })

	store, err := bucket.NewS3(ctx, storageCfg)
	if err != nil {
		t.Fatalf("montar adapter: %v", err)
	}

	// Instalação simulada: registro e pastas em disco REAL, como no harness da suíte — o que muda
	// aqui é só o armazenamento.
	root := t.TempDir()
	outboundDir := filepath.Join(root, "stcp", "SAIDA")
	backupDir := filepath.Join(root, "stcp", "BACKUP")
	logDir := filepath.Join(root, "stcp", "LOG")
	for _, d := range []string{outboundDir, backupDir, logDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("preparar %s: %v", d, err)
		}
	}

	led, err := ledger.NewFileLedger(filepath.Join(root, "ledger"))
	if err != nil {
		t.Fatalf("registro: %v", err)
	}
	sp, err := spool.NewDir(spool.Config{
		OutboundDir:     outboundDir,
		BackupDir:       backupDir,
		LogDir:          logDir,
		TransferLogGlob: "*.LOG",
	})
	if err != nil {
		t.Fatalf("pastas do cliente: %v", err)
	}

	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	fake := stcpfake.New(outboundDir, backupDir, filepath.Join(logDir, "20260818.LOG"))
	fake.Now = func() time.Time { return now }

	prefixes := prefixosDoAmbiente(t)
	ag, err := agent.New(store, led, fake, sp, agent.Config{
		Prefixes:    prefixes,
		NamePattern: namePattern,
		Clock:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("montar agente: %v", err)
	}

	// O emissor deposita a remessa — é o único sinal de "remessa pronta" que existe no contrato.
	if err := store.Put(ctx, prefixes.Outbound+remittanceName, []byte(remittanceContent)); err != nil {
		t.Fatalf("depositar remessa: %v", err)
	}

	sum, err := ag.TransmitCycle(ctx)
	if err != nil {
		t.Fatalf("ciclo abortou: %v", err)
	}
	if errs := sum.Errs(); len(errs) > 0 {
		t.Fatalf("o ciclo acumulou erros: %v", errs)
	}
	if len(sum.Outcomes) != 1 {
		t.Fatalf("esperava 1 desfecho, veio %d", len(sum.Outcomes))
	}
	if sum.Outcomes[0].Situation != envelope.Transmitted {
		t.Fatalf("situação = %q, esperava %q", sum.Outcomes[0].Situation, envelope.Transmitted)
	}

	// O status precisa estar legível NO BUCKET — é a única janela pela qual o core-api sabe o que
	// aconteceu.
	rawStatus, err := store.Get(ctx, envelope.Key(remittanceName))
	if err != nil {
		t.Fatalf("status ausente no bucket: %v", err)
	}
	var env envelope.Envelope
	if err := json.Unmarshal(rawStatus, &env); err != nil {
		t.Fatalf("status ilegível: %v", err)
	}
	if env.Situacao != envelope.Transmitted || env.Arquivo != remittanceName {
		t.Errorf("envelope publicado = %+v", env)
	}
	if env.LogTransferencia == nil {
		t.Error("logTransferencia veio null — o consumidor recusa o envelope inteiro")
	}

	// A localização É o estado (ADR-0061 §1): saiu da fila, está em processados.
	if _, err := store.Get(ctx, prefixes.Outbound+remittanceName); !errors.Is(err, bucket.ErrNotFound) {
		t.Errorf("o objeto continua no prefixo de saída; veio: %v", err)
	}
	if _, err := store.Get(ctx, prefixes.Processed+remittanceName); err != nil {
		t.Errorf("o objeto não chegou a processados: %v", err)
	}

	// E a evidência física que sustentou o veredito.
	if _, err := os.Stat(filepath.Join(backupDir, remittanceName)); err != nil {
		t.Errorf("o arquivo deveria estar em BACKUP: %v", err)
	}

	// Segunda passada com o mesmo nome: o cliente NÃO pode ser acionado de novo, e agora isso é
	// afirmado com o registro e o bucket reais entre as duas.
	acionamentos := len(fake.Calls())
	if err := store.Put(ctx, prefixes.Outbound+remittanceName, []byte(remittanceContent)); err != nil {
		t.Fatalf("redepositar remessa: %v", err)
	}
	if _, err := ag.TransmitCycle(ctx); err != nil {
		t.Fatalf("segundo ciclo abortou: %v", err)
	}
	if len(fake.Calls()) != acionamentos {
		t.Errorf("o cliente foi acionado na repetição: %d → %d", acionamentos, len(fake.Calls()))
	}
	if _, err := store.Get(ctx, envelope.DuplicateKey(remittanceName, now)); err != nil {
		t.Errorf("a tentativa repetida deveria publicar sob a chave de duplicado: %v", err)
	}
}

// prefixosDoAmbiente lê os prefixos pelo mesmo caminho que o binário usa.
func prefixosDoAmbiente(t *testing.T) bucket.Prefixes {
	t.Helper()
	// A leitura completa exige a configuração da máquina; aqui só os prefixos importam, e eles têm
	// default. Preencher o resto com caminhos temporários mantém o teste focado sem duplicar a
	// tabela de prefixos, que é justamente o que não pode divergir entre os dois lados.
	for k, v := range map[string]string{
		"VAN_AGENT_LEDGER_DIR":        t.TempDir(),
		"VAN_AGENT_NAME_PATTERN":      namePattern.String(),
		"VAN_AGENT_STCP_OUTBOUND_DIR": t.TempDir(),
		"VAN_AGENT_STCP_BACKUP_DIR":   t.TempDir(),
		"VAN_AGENT_STCP_LOG_DIR":      t.TempDir(),
		"VAN_AGENT_STCP_EXE":          "stcpclt-ficticio",
		"VAN_AGENT_STCP_INI":          "stcp-ficticio.ini",
		"VAN_AGENT_STCP_PROFILE":      "PERFIL-DE-TESTE",
	} {
		t.Setenv(k, v)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("ler configuração da máquina: %v", err)
	}
	return cfg.Prefixes
}

func clienteDeApoio(t *testing.T, ctx context.Context, cfg bucket.S3Config) *s3.Client {
	t.Helper()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.Credentials.AccessKeyID, cfg.Credentials.SecretAccessKey, "")),
	)
	if err != nil {
		t.Fatalf("configurar cliente de apoio: %v", err)
	}
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.Endpoint)
		o.UsePathStyle = cfg.ForcePathStyle
	})
}

// limpaBucket usa o cliente CRU, e não o adapter: `bucket.Store` não tem remoção de propósito
// (ADR-0061 §1), e abrir a exceção "só para limpar o teste" deixaria o método ao alcance do ciclo.
func limpaBucket(t *testing.T, raw *s3.Client, bucketName string) {
	t.Helper()
	ctx := context.Background()
	pager := s3.NewListObjectsV2Paginator(raw, &s3.ListObjectsV2Input{Bucket: aws.String(bucketName)})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			t.Logf("listar para limpeza: %v", err)
			return
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			if _, err := raw.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(bucketName),
				Key:    obj.Key,
			}); err != nil {
				t.Logf("remover %q: %v", *obj.Key, err)
			}
		}
	}
	if _, err := raw.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucketName)}); err != nil {
		t.Logf("remover bucket de teste: %v", err)
	}
}
