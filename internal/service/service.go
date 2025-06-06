package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/SwanHtetAungPhyo/kyc-api/internal/models"
	"github.com/SwanHtetAungPhyo/kyc-api/internal/repo"
	"github.com/SwanHtetAungPhyo/kyc-api/pkg/logger"
	"github.com/aws/aws-sdk-go-v2/service/rekognition"
)

type KYCService interface {
	VerifyKYC(ctx context.Context, idBlob, selfieBlob []byte, email string) (*models.VerificationResult, error)
	CheckIfProceed(ctx context.Context, email string) (bool, error)
}

type kycService struct {
	awsRepo  repo.AWSRepository
	logger   logger.Logger
	criteria models.FaceValidationCriteria
}

func NewKYCService(awsRepo repo.AWSRepository, log logger.Logger) KYCService {
	return &kycService{
		awsRepo:  awsRepo,
		logger:   log,
		criteria: models.DefaultFaceValidationCriteria(),
	}
}

func (s *kycService) VerifyKYC(ctx context.Context, idBlob, selfieBlob []byte, email string) (*models.VerificationResult, error) {
	s.logger.WithField("email", email).Info("Starting KYC verification")

	if err := s.validateInput(idBlob, selfieBlob); err != nil {
		return nil, err
	}

	if err := s.analyzeIDDocument(ctx, idBlob); err != nil {
		s.logger.WithError(err).Error("ID document analysis failed")
		return nil, fmt.Errorf("ID analysis failed: %w", err)
	}

	_, err := s.detectAndValidateFaces(ctx, selfieBlob)
	if err != nil {
		s.logger.WithError(err).Error("Face detection/validation failed")
		return nil, fmt.Errorf("face validation failed: %w", err)
	}

	similarity, err := s.compareFaces(ctx, idBlob, selfieBlob)
	if err != nil {
		s.logger.WithError(err).Error("Face comparison failed")
		return nil, fmt.Errorf("face comparison failed: %w", err)
	}

	verified := similarity >= s.criteria.MinSimilarity
	message := s.generateVerificationMessage(verified, similarity)

	if err := s.awsRepo.RecordAttempt(ctx, email, verified); err != nil {
		s.logger.WithError(err).Error("Failed to record KYC attempt")
	}

	result := &models.VerificationResult{
		Verified:   verified,
		Similarity: similarity,
		Message:    message,
	}

	s.logger.WithFields(map[string]interface{}{
		"email":      email,
		"verified":   verified,
		"similarity": similarity,
	}).Info("KYC verification completed")

	return result, nil
}

func (s *kycService) CheckIfProceed(ctx context.Context, email string) (bool, error) {
	return s.awsRepo.CheckIfProceed(ctx, email)
}

func (s *kycService) validateInput(idBlob, selfieBlob []byte) error {
	if len(idBlob) == 0 {
		return errors.New("ID image data is empty")
	}
	if len(selfieBlob) == 0 {
		return errors.New("selfie image data is empty")
	}
	return nil
}

func (s *kycService) analyzeIDDocument(ctx context.Context, idBlob []byte) error {
	_, err := s.awsRepo.AnalyzeID(ctx, idBlob)
	if err != nil {
		return fmt.Errorf("textract analysis failed: %w", err)
	}

	s.logger.Debug("ID document analysis completed successfully")
	return nil
}

func (s *kycService) detectAndValidateFaces(ctx context.Context, selfieBlob []byte) (*rekognition.DetectFacesOutput, error) {
	faces, err := s.awsRepo.DetectFaces(ctx, selfieBlob)
	if err != nil {
		return nil, err
	}

	if err := s.validateFaceQuality(faces); err != nil {
		return nil, err
	}

	return faces, nil
}

func (s *kycService) validateFaceQuality(faces *rekognition.DetectFacesOutput) error {
	if len(faces.FaceDetails) != 1 {
		s.logger.WithField("face_count", len(faces.FaceDetails)).Error("Invalid number of faces detected")
		return fmt.Errorf("exactly one face should be detected, found %d", len(faces.FaceDetails))
	}

	face := faces.FaceDetails[0]

	if face.Confidence == nil || *face.Confidence < s.criteria.MinConfidence {
		confidence := float32(0)
		if face.Confidence != nil {
			confidence = *face.Confidence
		}
		s.logger.WithField("confidence", confidence).Error("Low face detection confidence")
		return fmt.Errorf("low face detection confidence: %.2f (required: %.2f)",
			confidence, s.criteria.MinConfidence)
	}

	if face.Quality == nil {
		return errors.New("missing face quality data")
	}

	if face.Quality.Brightness == nil || face.Quality.Sharpness == nil {
		return errors.New("incomplete face quality metrics")
	}

	brightness := *face.Quality.Brightness
	sharpness := *face.Quality.Sharpness

	if brightness < s.criteria.MinBrightness || sharpness < s.criteria.MinSharpness {
		s.logger.WithFields(map[string]interface{}{
			"brightness": brightness,
			"sharpness":  sharpness,
		}).Error("Poor image quality")
		return fmt.Errorf("poor image quality (brightness: %.2f/%.2f, sharpness: %.2f/%.2f)",
			brightness, s.criteria.MinBrightness, sharpness, s.criteria.MinSharpness)
	}

	s.logger.WithFields(map[string]interface{}{
		"confidence": *face.Confidence,
		"brightness": brightness,
		"sharpness":  sharpness,
	}).Info("Face validation passed")

	return nil
}

func (s *kycService) compareFaces(ctx context.Context, idBlob, selfieBlob []byte) (float32, error) {
	compareResult, err := s.awsRepo.CompareFaces(ctx, idBlob, selfieBlob, s.criteria.MinSimilarity)
	if err != nil {
		return 0, err
	}

	if len(compareResult.FaceMatches) == 0 {
		s.logger.Error("No face matches found")
		return 0, errors.New("no face matches found")
	}

	similarity := *compareResult.FaceMatches[0].Similarity
	s.logger.WithField("similarity", similarity).Info("Face comparison completed")

	return similarity, nil
}

func (s *kycService) generateVerificationMessage(verified bool, similarity float32) string {
	if verified {
		return fmt.Sprintf("KYC verification successful with %.2f%% similarity", similarity)
	}
	return fmt.Sprintf("KYC verification failed with %.2f%% similarity (required: %.2f%%)",
		similarity, s.criteria.MinSimilarity)
}
