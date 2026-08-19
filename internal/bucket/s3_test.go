package bucket_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/ERP-Bem-Comum/van-agent/internal/bucket"
	"github.com/ERP-Bem-Comum/van-agent/internal/config"
)

func TestS3ConfigRecusaConfiguracaoIncompleta(t *testing.T) {
	casos := []struct {
		nome string
		cfg  bucket.S3Config
	}{
		{"sem bucket", bucket.S3Config{Region: "us-east-1"}},
		{"sem região", bucket.S3Config{Bucket: "b"}},
		{
			"credencial pela metade",
			bucket.S3Config{
				Bucket:      "b",
				Region:      "us-east-1",
				Credentials: &bucket.StaticCredentials{AccessKeyID: "AKIAFICTICIO"},
			},
		},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			if err := c.cfg.Validate(); err == nil {
				t.Error("configuração incompleta deveria ser recusada antes de qualquer chamada de rede")
			}
		})
	}
}

// A redação viaja com o tipo, e não com quem formata: é o que faz a proteção sobreviver a um
// `%v` escrito às pressas para depurar um boot que falhou na máquina.
func TestCredencialNaoVazaAoSerFormatada(t *testing.T) {
	cfg := bucket.S3Config{
		Bucket: "bucket-ficticio",
		Region: "us-east-1",
		Credentials: &bucket.StaticCredentials{
			AccessKeyID:     "AKIAFICTICIO",
			SecretAccessKey: "segredo-que-nao-pode-aparecer",
		},
	}
	for _, formatado := range []string{
		fmt.Sprintf("%v", cfg),
		fmt.Sprintf("%+v", cfg),
		fmt.Sprintf("%v", *cfg.Credentials),
	} {
		if strings.Contains(formatado, "segredo-que-nao-pode-aparecer") {
			t.Errorf("o segredo apareceu na formatação: %s", formatado)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CA7 — o adapter real, contra um endpoint S3-compatível
//
// Pulado (nunca falho) quando não há endpoint configurado: a suíte precisa continuar rodável numa
// máquina sem nuvem, sem container e sem rede — que é a condição em que ela roda hoje.
// ─────────────────────────────────────────────────────────────────────────────

func TestCA7_AdapterRealExercitaOsQuatroMetodos(t *testing.T) {
	endpoint := os.Getenv("VAN_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("VAN_S3_ENDPOINT não configurado — sem endpoint S3-compatível para exercitar o adapter")
	}
	keyID, secret := os.Getenv("VAN_S3_ACCESS_KEY_ID"), os.Getenv("VAN_S3_SECRET_ACCESS_KEY")
	if keyID == "" || secret == "" {
		t.Skip("credencial do endpoint local não configurada — o teste não inventa chave")
	}

	ctx := context.Background()
	// Nome derivado do relógio: cada execução usa um bucket próprio, para que uma passada anterior
	// interrompida não deixe objeto capaz de mudar o resultado da seguinte.
	bucketName := fmt.Sprintf("van-agent-teste-%d", time.Now().UnixNano())
	t.Setenv("VAN_S3_BUCKET", bucketName)

	// A configuração vem do AMBIENTE, pelo mesmo caminho que o binário usa, e não de uma struct
	// montada aqui. É o que faz este teste cobrir a fronteira inteira: uma regra de normalização de
	// prefixo ou de decisão de path-style que estivesse errada só apareceria assim.
	cfg, err := config.LoadStorage()
	if err != nil {
		t.Fatalf("ler configuração do ambiente: %v", err)
	}
	if !cfg.ForcePathStyle {
		t.Fatalf("endpoint %q exige endereçamento por caminho; defina VAN_S3_FORCE_PATH_STYLE=true", endpoint)
	}

	raw := rawClient(t, ctx, cfg)
	if _, err := raw.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucketName)}); err != nil {
		t.Fatalf("criar bucket de teste: %v", err)
	}
	t.Cleanup(func() { esvaziaEApaga(t, raw, bucketName) })

	store, err := bucket.NewS3(ctx, cfg)
	if err != nil {
		t.Fatalf("montar adapter: %v", err)
	}

	prefixes := bucket.DefaultPrefixes()
	const nome = "PAG_000000_000001.REM"
	conteudo := []byte("0371234...CONTEUDO CNAB FICTICIO...")

	// Put
	if err := store.Put(ctx, prefixes.Outbound+nome, conteudo); err != nil {
		t.Fatalf("escrever: %v", err)
	}

	// List — e a listagem por prefixo não pode trazer objeto de prefixo vizinho.
	if err := store.Put(ctx, prefixes.Status+"ruido.json", []byte("{}")); err != nil {
		t.Fatalf("escrever objeto vizinho: %v", err)
	}
	keys, err := store.List(ctx, prefixes.Outbound)
	if err != nil {
		t.Fatalf("listar: %v", err)
	}
	if len(keys) != 1 || keys[0] != prefixes.Outbound+nome {
		t.Fatalf("listagem = %v, esperava só %q", keys, prefixes.Outbound+nome)
	}

	// Get — byte a byte. O agente deposita na pasta de SAÍDA o que leu daqui, e um único byte
	// alterado é um arquivo que o banco recusa sem que ninguém saiba por quê.
	lido, err := store.Get(ctx, prefixes.Outbound+nome)
	if err != nil {
		t.Fatalf("ler: %v", err)
	}
	if string(lido) != string(conteudo) {
		t.Errorf("conteúdo lido difere do escrito:\n  escrito: %q\n  lido:    %q", conteudo, lido)
	}

	// Get de chave ausente é ErrNotFound, não indisponibilidade: a diferença decide se o ciclo
	// segue em frente ou alarma.
	if _, err := store.Get(ctx, prefixes.Outbound+"NAO-EXISTE.REM"); !errors.Is(err, bucket.ErrNotFound) {
		t.Errorf("chave ausente deveria virar ErrNotFound; veio: %v", err)
	}

	// Move — a origem some, o destino aparece, e o conteúdo atravessa íntegro.
	if err := store.Move(ctx, prefixes.Outbound+nome, prefixes.Processed+nome); err != nil {
		t.Fatalf("mover: %v", err)
	}
	if _, err := store.Get(ctx, prefixes.Outbound+nome); !errors.Is(err, bucket.ErrNotFound) {
		t.Errorf("a origem deveria ter saído do prefixo de saída; veio: %v", err)
	}
	movido, err := store.Get(ctx, prefixes.Processed+nome)
	if err != nil {
		t.Fatalf("ler o objeto movido: %v", err)
	}
	if string(movido) != string(conteudo) {
		t.Errorf("o conteúdo mudou na movimentação:\n  antes: %q\n  depois: %q", conteudo, movido)
	}

	// Mover o que não existe é ErrNotFound — e não uma cópia vazia criada no destino.
	if err := store.Move(ctx, prefixes.Outbound+"NAO-EXISTE.REM", prefixes.Failed+"NAO-EXISTE.REM"); !errors.Is(err, bucket.ErrNotFound) {
		t.Errorf("mover objeto ausente deveria virar ErrNotFound; veio: %v", err)
	}
}

func rawClient(t *testing.T, ctx context.Context, cfg bucket.S3Config) *s3.Client {
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
		o.UsePathStyle = true
	})
}

// esvaziaEApaga limpa o bucket de teste.
//
// A remoção mora AQUI, no cliente de apoio do teste, e não no adapter: `bucket.Store` não tem
// `Delete` de propósito (ADR-0061 §1), e abrir uma exceção "só para o teste" no código de produção
// deixaria o método disponível para o ciclo.
func esvaziaEApaga(t *testing.T, raw *s3.Client, bucketName string) {
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
